package kube

// The exec transport: a real WebSocket upgrade against the fake API
// server, speaking v4.channel.k8s.io from the server side.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/docker"
)

// wsServerFrame writes one unmasked server frame.
func wsServerFrame(w io.Writer, opcode byte, payload []byte) error {
	hdr := []byte{0x80 | opcode}
	n := len(payload)
	switch {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		hdr = append(hdr, b[:]...)
	default:
		hdr = append(hdr, 127)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		hdr = append(hdr, b[:]...)
	}
	if _, err := w.Write(append(hdr, payload...)); err != nil {
		return err
	}
	return nil
}

// v4Frame prefixes a channel byte (the v4 exec framing).
func v4Frame(w io.Writer, channel byte, payload []byte) error {
	return wsServerFrame(w, 0x2, append([]byte{channel}, payload...))
}

// installExecHandler teaches the fake server a working exec endpoint:
// it echoes the scripted stdout/stderr, reports the status JSON on the
// error channel, then closes.
func installExecHandler(f *fakeAPI, stdout, stderr string, status map[string]any) {
	f.execFn = func(w http.ResponseWriter, r *http.Request, pod string) {
		if _, ok := f.pods[pod]; !ok {
			writeStatus(w, http.StatusNotFound, "NotFound", "pods \""+pod+"\" not found")
			return
		}
		if strings.ToLower(r.Header.Get("Upgrade")) != "websocket" {
			http.Error(w, "expected websocket upgrade", http.StatusBadRequest)
			return
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		key := r.Header.Get("Sec-WebSocket-Key")
		resp := strings.Join([]string{
			"HTTP/1.1 101 Switching Protocols",
			"Upgrade: websocket",
			"Connection: Upgrade",
			"Sec-WebSocket-Protocol: " + r.Header.Get("Sec-WebSocket-Protocol"),
			"Sec-WebSocket-Accept: " + wsAccept(key),
			"", "",
		}, "\r\n")
		if _, err := conn.Write([]byte(resp)); err != nil {
			return
		}
		if stdout != "" {
			_ = v4Frame(conn, v4Channels.Stdout, []byte(stdout))
		}
		if stderr != "" {
			_ = v4Frame(conn, v4Channels.Stderr, []byte(stderr))
		}
		if status != nil {
			b, _ := json.Marshal(status)
			_ = v4Frame(conn, v4Channels.Error, b)
		}
		_ = wsServerFrame(conn, 0x8, nil) // close
		// Give the client a moment to drain before the TCP close.
		_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
		time.Sleep(50 * time.Millisecond)
	}
}

func TestExecOverWebSocket(t *testing.T) {
	f := newFakeAPI()
	installExecHandler(f, "hello from the pod\n", "a warning\n",
		map[string]any{"status": "Success"})
	eng, _ := newTestEngine(t, f, nil)
	ctx := context.Background()
	if _, err := eng.Run(ctx, sampleSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	res, err := eng.Exec(ctx, "s-01jtest", []string{"sh", "-c", "echo hi"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.Stdout != "hello from the pod\n" || res.Stderr != "a warning\n" {
		t.Errorf("exec result = %+v", res)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}

func TestExecExitCodeFromStatus(t *testing.T) {
	f := newFakeAPI()
	installExecHandler(f, "", "boom\n", map[string]any{
		"status": "Failure", "reason": "NonZeroExitCode",
		"details": map[string]any{
			"causes": []map[string]string{
				{"reason": "ExitCode", "message": "42"},
			},
		},
	})
	eng, _ := newTestEngine(t, f, nil)
	ctx := context.Background()
	if _, err := eng.Run(ctx, sampleSpec()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	res, err := eng.Exec(ctx, "s-01jtest", []string{"false"})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 42 {
		t.Errorf("exit code = %d, want 42 (from the status causes)", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "boom") {
		t.Errorf("stderr = %q", res.Stderr)
	}
}

func TestExecMissingPod(t *testing.T) {
	eng, _ := newTestEngine(t, newFakeAPI(), nil)
	_, err := eng.Exec(context.Background(), "s-absent", []string{"true"})
	if !errors.Is(err, docker.ErrNotFound) {
		t.Errorf("Exec missing pod = %v, want docker.ErrNotFound", err)
	}
}

func TestExecRespectsContext(t *testing.T) {
	f := newFakeAPI()
	f.execFn = func(w http.ResponseWriter, r *http.Request, pod string) {
		// Never completes the upgrade: hangs as a plain HTTP response.
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}
	eng, _ := newTestEngine(t, f, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if _, err := eng.Exec(ctx, "s-01jtest", []string{"true"}); err == nil {
		t.Errorf("Exec on a hung server must fail with the ctx error")
	}
}
