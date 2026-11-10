package kube

// A fake Kubernetes API server for the engine tests: just enough of the
// core/v1 pod surface, the exec subresource (real WebSocket upgrade) and
// the sidecar endpoints to exercise the adapter end-to-end in-process.

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI records what the engine did and serves a tiny pod store.
type fakeAPI struct {
	mu          sync.Mutex
	pods        map[string]Pod
	created     map[string]int // "pods/x", "services/x", "netpols/x", "ingresses/x" -> count
	deleted     map[string]int
	execFn      func(w http.ResponseWriter, r *http.Request, pod string)
	logBody     string
	deleteDelay time.Duration // pods only: simulate the kubelet grace period
}

func newFakeAPI() *fakeAPI {
	return &fakeAPI{
		pods:    map[string]Pod{},
		created: map[string]int{},
		deleted: map[string]int{},
	}
}

func (f *fakeAPI) counts(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.created[key]
}

func (f *fakeAPI) del(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleted[key]
}

func (f *fakeAPI) handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(Version{Major: "1", Minor: "31", GitVersion: "v1.31.4+k3s1"})
	})
	// networking.k8s.io/v1 collection endpoints (NetworkPolicy, Ingress).
	m.HandleFunc("/apis/networking.k8s.io/v1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/apis/networking.k8s.io/v1/namespaces/"), "/")
		rest := strings.Split(path, "/")
		if len(rest) < 2 {
			http.NotFound(w, r)
			return
		}
		kind := rest[1]
		name := ""
		if len(rest) > 2 {
			name = rest[2]
		}
		body, _ := io.ReadAll(r.Body)
		switch r.Method {
		case http.MethodPost:
			var probe struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			json.Unmarshal(body, &probe)
			key := kind + "/"
			if probe.Metadata.Name != "" {
				key += probe.Metadata.Name
			} else {
				key += "?"
			}
			f.created[key]++
			w.WriteHeader(http.StatusCreated)
		case http.MethodDelete:
			f.deleted[kind+"/"+name]++
			json.NewEncoder(w).Encode(map[string]any{"status": "Success"})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	m.HandleFunc("/api/v1/namespaces/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		path := strings.TrimPrefix(r.URL.Path, "/api/v1/namespaces/")
		if path == "test/pods" && r.Method == http.MethodGet && r.URL.Query().Get("labelSelector") != "" {
			var items []Pod
			for _, p := range f.pods {
				items = append(items, p)
			}
			json.NewEncoder(w).Encode(PodList{Items: items})
			return
		}
		rest := strings.Split(strings.Trim(path, "/"), "/")
		// rest = [ns, kind] (collection) or [ns, kind, name] or [ns, kind, name, sub]
		if len(rest) < 2 {
			http.NotFound(w, r)
			return
		}
		kind := rest[1]
		name := ""
		if len(rest) > 2 {
			name = rest[2]
		}
		sub := ""
		if len(rest) > 3 {
			sub = rest[3]
		}
		body, _ := io.ReadAll(r.Body)
		objName := func() string {
			var probe struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			}
			if json.Unmarshal(body, &probe) == nil {
				return probe.Metadata.Name
			}
			return "?"
		}()
		if sub == "" && name == "" && r.Method == http.MethodPost {
			// Collection create: pods carry their name in the body.
			switch kind {
			case "pods":
				var p Pod
				json.Unmarshal(body, &p)
				if _, exists := f.pods[p.Metadata.Name]; exists {
					writeStatus(w, http.StatusConflict, "AlreadyExists", "")
					return
				}
				// Simulate the scheduler: created pods go Pending; tests
				// flip them to their desired phase via setPodPhase.
				p.Status.Phase = "Pending"
				f.pods[p.Metadata.Name] = p
				f.created["pods/"+p.Metadata.Name]++
			default:
				f.created[kind+"/"+objName]++
			}
			w.WriteHeader(http.StatusCreated)
			return
		}
		bucket := kind
		if sub == "exec" && kind == "pods" {
			if _, ok := f.pods[name]; !ok {
				writeStatus(w, http.StatusNotFound, "NotFound", "pods \""+name+"\" not found")
				return
			}
			if f.execFn != nil {
				f.execFn(w, r, name)
			} else {
				http.Error(w, "no exec", http.StatusNotImplemented)
			}
			return
		}
		if kind == "pods" && sub == "log" {
			if _, ok := f.pods[name]; !ok {
				writeStatus(w, http.StatusNotFound, "NotFound", "pods \""+name+"\" not found")
				return
			}
			fmt.Fprint(w, f.logBody)
			return
		}
		_ = bucket
		switch r.Method {
		case http.MethodGet:
			if kind == "pods" {
				p, ok := f.pods[name]
				if !ok {
					writeStatus(w, http.StatusNotFound, "NotFound", "pods \""+name+"\" not found")
					return
				}
				json.NewEncoder(w).Encode(p)
				return
			}
			writeStatus(w, http.StatusNotFound, "NotFound", kind+" \""+name+"\" not found")
		case http.MethodDelete:
			f.deleted[bucket+"/"+name]++
			if kind == "pods" {
				if _, ok := f.pods[name]; !ok {
					writeStatus(w, http.StatusNotFound, "NotFound", "pods \""+name+"\" not found")
					return
				}
				if f.deleteDelay > 0 {
					f.mu.Unlock()
					time.Sleep(f.deleteDelay)
					f.mu.Lock()
				}
				delete(f.pods, name)
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "Success"})
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	})
	return m
}

func writeStatus(w http.ResponseWriter, code int, reason, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"kind": "Status", "status": "Failure", "reason": reason, "message": msg, "code": code,
	})
}

// setPodPhase moves a pod (as the cluster would).
func (f *fakeAPI) setPodPhase(name, phase, ip string, running bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.pods[name]
	if !ok {
		return
	}
	p.Status.Phase = phase
	p.Status.PodIP = ip
	if running {
		p.Status.ContainerStatuses = []ContainerStatus{{
			Name: "sandbox",
			State: ContainerState{Running: &struct {
				StartedAt string `json:"startedAt,omitempty"`
			}{StartedAt: "now"}},
		}}
	}
	f.pods[name] = p
}

// newTestEngine wires an Engine to a fake API server.
func newTestEngine(t *testing.T, f *fakeAPI, mutate func(*Engine)) (*Engine, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	cfg, err := LoadConfig(Config{APIServer: srv.URL, Insecure: true, Namespace: "test"})
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	eng, err := New(cfg, t.TempDir(), nil)
	if err != nil {
		t.Fatalf("new engine: %v", err)
	}
	if mutate != nil {
		mutate(eng)
	}
	return eng, srv
}

// wsAccept computes the Sec-WebSocket-Accept value for a client key.
func wsAccept(key string) string {
	h := sha1.New()
	h.Write([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
