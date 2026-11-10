package netguard

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGuard(t *testing.T) {
	var g Guard
	if g.Denied("172.18.0.5:1234") {
		t.Fatal("zero guard must allow everything")
	}
	if bad := g.Set([]string{"172.18.0.0/16", "nonsense"}, []string{"172.18.0.1"}); len(bad) != 1 {
		t.Fatalf("bad = %v", bad)
	}
	cases := map[string]bool{
		"172.18.0.5:1234":        true,  // a sandbox
		"[::ffff:172.18.3.4]:80": true,  // v4-mapped sandbox
		"172.18.0.1:5555":        false, // gateway = the host
		"127.0.0.1:9000":         false,
		"172.19.0.2:80":          false, // control network
		"not-an-addr":            false,
	}
	for addr, want := range cases {
		if got := g.Denied(addr); got != want {
			t.Errorf("Denied(%q) = %v, want %v", addr, got, want)
		}
	}
	h := g.Wrap(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
	req := httptest.NewRequest("GET", "/v1/sandboxes", nil)
	req.RemoteAddr = "172.18.0.9:4000"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("sandbox source got %d, want 403", rec.Code)
	}
	req.RemoteAddr = "127.0.0.1:4000"
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("loopback got %d, want 204", rec.Code)
	}
}
