// Package kube implements the M8 (k3s B2, PLAN §2) Kubernetes runtime
// adapter: a sandbox becomes a Pod, with a Service (its preview ports),
// a NetworkPolicy (egress deny-by-default) and — when a preview domain
// is configured — an Ingress (host-based routing).
//
// The engine satisfies docker.Engine, so a sandboxd executor started
// with SANDBOXD_EXECUTOR_ENGINE=kubernetes runs every sandbox on the
// cluster its own pod sits in, and the rest of sandboxd (store, policy,
// API, fleet) is unchanged.
//
// Deliberately NO client-go: the adapter speaks the REST API it needs
// with net/http and a hand-rolled WebSocket for exec, keeping the
// module's dependency set unchanged. The Kubernetes objects below are
// the minimal shapes we marshal; unknown fields are ignored by
// encoding/json on the way in.
package kube

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is everything the engine needs to reach the cluster.
type Config struct {
	// APIServer is the cluster endpoint (https://host:port). Empty is
	// filled from in-cluster env or kubeconfig, in that order.
	APIServer string
	// Token is the bearer token of the executor's ServiceAccount.
	Token string
	// CAData is the cluster CA bundle (PEM). Empty + Insecure=false uses
	// the system roots (fine for k3s only when CAData is provided).
	CAData []byte
	// Namespace is where sandbox pods are created. Empty = the
	// ServiceAccount namespace, else "default".
	Namespace string
	// Insecure skips TLS verification — a dev convenience for plain-http
	// or self-signed clusters; never needed in-cluster.
	Insecure bool
}

// Service-account mount points (vars so tests can point them at fixtures).
var (
	inClusterTokenPath = "/var/run/secrets/kubernetes.io/serviceaccount/token"
	inClusterRootCA    = "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt"
	inClusterNSPath    = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// LoadConfig resolves the cluster connection, in priority order:
//
//  1. explicit fields on cfg (the env wiring in main.go),
//  2. the kubeconfig named by SANDBOXD_KUBECONFIG (or KUBECONFIG, or
//     ~/.kube/config) — the current context's server, token and CA,
//  3. in-cluster ServiceAccount config (the normal case for the M8
//     executor running as a pod).
//
// The namespace resolves in the same order (explicit > kubeconfig
// context namespace > in-cluster namespace file > "default").
func LoadConfig(cfg Config) (Config, error) {
	if cfg.APIServer == "" {
		if kc := kubeconfigPath(); kc != "" {
			if c, err := loadKubeconfig(kc); err == nil {
				cfg.APIServer = c.APIServer
				if cfg.Token == "" {
					cfg.Token = c.Token
				}
				if len(cfg.CAData) == 0 {
					cfg.CAData = c.CAData
				}
				if cfg.Namespace == "" {
					cfg.Namespace = c.Namespace
				}
			}
		}
	}
	if cfg.APIServer == "" {
		ic, err := inClusterConfig()
		if err != nil {
			return cfg, fmt.Errorf("kube: no cluster connection (set SANDBOXD_K8S_API / SANDBOXD_KUBECONFIG, or run in-cluster): %w", err)
		}
		if cfg.Token == "" {
			cfg.Token = ic.Token
		}
		if len(cfg.CAData) == 0 {
			cfg.CAData = ic.CAData
		}
		if cfg.Namespace == "" {
			cfg.Namespace = ic.Namespace
		}
		cfg.APIServer = ic.APIServer
	}
	if cfg.Namespace == "" {
		if b, err := os.ReadFile(inClusterNSPath); err == nil && strings.TrimSpace(string(b)) != "" {
			cfg.Namespace = strings.TrimSpace(string(b))
		} else {
			cfg.Namespace = "default"
		}
	}
	cfg.APIServer = strings.TrimRight(cfg.APIServer, "/")
	if _, err := url.Parse(cfg.APIServer); err != nil || cfg.APIServer == "" {
		return cfg, fmt.Errorf("kube: bad api server %q", cfg.APIServer)
	}
	if strings.HasPrefix(cfg.APIServer, "https://") && !cfg.Insecure && len(cfg.CAData) == 0 {
		return cfg, errors.New("kube: https api server without a CA bundle (set SANDBOXD_K8S_CA or SANDBOXD_K8S_INSECURE=true for dev)")
	}
	return cfg, nil
}

// inClusterConfig reads the standard ServiceAccount mount.
func inClusterConfig() (Config, error) {
	host, port := os.Getenv("KUBERNES_SERVICE_HOST"), os.Getenv("KUBERNES_SERVICE_PORT")
	if host == "" || port == "" {
		return Config{}, errors.New("not running in-cluster (KUBERNES_SERVICE_HOST/PORT unset)")
	}
	tok, err := os.ReadFile(inClusterTokenPath)
	if err != nil {
		return Config{}, fmt.Errorf("read service account token: %w", err)
	}
	cfg := Config{
		APIServer: "https://" + host + ":" + port,
		Token:     strings.TrimSpace(string(tok)),
	}
	if ca, err := os.ReadFile(inClusterRootCA); err == nil {
		cfg.CAData = ca
	}
	if ns, err := os.ReadFile(inClusterNSPath); err == nil {
		cfg.Namespace = strings.TrimSpace(string(ns))
	}
	return cfg, nil
}

// kubeconfigPath picks the kubeconfig to try ("" = none).
func kubeconfigPath() string {
	if p := os.Getenv("SANDBOXD_KUBECONFIG"); p != "" {
		return p
	}
	if p := os.Getenv("KUBECONFIG"); p != "" {
		return strings.Split(p, ":")[0]
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".kube", "config")
}

// kubeconfigFile is the tiny slice of a kubeconfig we understand:
// current-context -> cluster (server, CA) and user (token). Client
// certificates and exec-plugin users are not supported — an executor
// uses a ServiceAccount token.
type kubeconfigFile struct {
	CurrentContext string `yaml:"current-context"`
	Contexts       []struct {
		Name    string `yaml:"name"`
		Context struct {
			Cluster   string `yaml:"cluster"`
			User      string `yaml:"user"`
			Namespace string `yaml:"namespace"`
		} `yaml:"context"`
	} `yaml:"contexts"`
	Clusters []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server string `yaml:"server"`
			CAData string `yaml:"certificate-authority-data"`
			CAFile string `yaml:"certificate-authority"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
	Users []struct {
		Name string `yaml:"name"`
		User struct {
			Token string `yaml:"token"`
		} `yaml:"user"`
	} `yaml:"users"`
}

func loadKubeconfig(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var f kubeconfigFile
	if err := yaml.Unmarshal(b, &f); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var out Config
	cur := f.CurrentContext
	for _, c := range f.Contexts {
		if c.Name == cur {
			out.Namespace = c.Context.Namespace
			cur = c.Context.Cluster
			break
		}
	}
	for _, c := range f.Clusters {
		if c.Name == cur {
			out.APIServer = c.Cluster.Server
			if d := c.Cluster.CAData; d != "" {
				out.CAData = decodeBase64Pad(d)
			} else if c.Cluster.CAFile != "" {
				out.CAData, _ = os.ReadFile(c.Cluster.CAFile)
			}
			break
		}
	}
	for _, u := range f.Users {
		if out.Token == "" && u.User.Token != "" {
			out.Token = u.User.Token
		}
	}
	if out.APIServer == "" {
		return out, fmt.Errorf("kubeconfig %s: no server for context", path)
	}
	return out, nil
}

// decodeBase64Pad decodes a possibly unpadded base64 blob (kubernetes
// emits standard padded base64, but hand-edited kubeconfigs often drop
// the padding).
func decodeBase64Pad(s string) []byte {
	s = strings.TrimSpace(s)
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	if b, err := base64.RawStdEncoding.DecodeString(s); err == nil {
		return b
	}
	return nil
}

// Client is a minimal Kubernetes REST client: JSON in/out on fixed
// paths plus one streaming path (pod logs).
type Client struct {
	base *url.URL
	http *http.Client
	tok  string
	ns   string
}

// NewClient builds the REST client from a resolved Config.
func NewClient(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.APIServer)
	if err != nil {
		return nil, fmt.Errorf("kube: parse api server: %w", err)
	}
	tr := &http.Transport{}
	if u.Scheme == "https" {
		tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
		if cfg.Insecure {
			tlsCfg.InsecureSkipVerify = true
		} else if len(cfg.CAData) > 0 {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(cfg.CAData) {
				return nil, errors.New("kube: cluster CA bundle contains no certificates")
			}
			tlsCfg.RootCAs = pool
		}
		tr.TLSClientConfig = tlsCfg
	}
	return &Client{
		base: u,
		http: &http.Client{Transport: tr, Timeout: 5 * time.Minute},
		tok:  cfg.Token,
		ns:   cfg.Namespace,
	}, nil
}

// StatusError is a non-2xx API response (the Status object's message
// when parseable, else the body).
type StatusError struct {
	Code    int
	Reason  string
	Message string
}

func (e *StatusError) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return "kubernetes api " + strconv.Itoa(e.Code)
}

// IsNotFound reports whether err is the API's 404.
func IsNotFound(err error) bool {
	var se *StatusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// Namespace returns the namespace objects are created in.
func (c *Client) Namespace() string { return c.ns }

// do issues one JSON request. in (any non-nil value) marshals to the
// body; out (if non-nil) unmarshals the response.
func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("kube: encode %s: %w", path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base.String()+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		se := &StatusError{Code: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		var st struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &st) == nil {
			se.Reason = st.Reason
			se.Message = st.Message
		}
		return se
	}
	if out != nil && len(raw) > 0 {
		return json.Unmarshal(raw, out)
	}
	return nil
}

// stream opens a long-lived GET (pod logs) and returns the raw body.
// The caller MUST Close it.
func (c *Client) stream(ctx context.Context, path string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base.String()+path, nil)
	if err != nil {
		return nil, err
	}
	if c.tok != "" {
		req.Header.Set("Authorization", "Bearer "+c.tok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		se := &StatusError{Code: resp.StatusCode, Message: strings.TrimSpace(string(raw))}
		var st struct {
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &st) == nil && st.Message != "" {
			se.Message = st.Message
		}
		return nil, se
	}
	return resp.Body, nil
}
