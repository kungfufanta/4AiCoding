package egressproxy

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/policy"
)

func TestBlocked(t *testing.T) {
	for s, want := range map[string][2]bool{
		"127.0.0.1":       {true, false},
		"169.254.169.254": {true, false},
		"0.0.0.0":         {true, false},
		"100.64.0.1":      {true, false},
		"::1":             {true, false},
		"fe80::1":         {true, false},
		"10.1.2.3":        {false, true},
		"192.168.1.1":     {false, true},
		"fd00::1":         {false, true},
		"1.1.1.1":         {false, false},
	} {
		never, private := Blocked(netip.MustParseAddr(s))
		if never != want[0] || private != want[1] {
			t.Errorf("Blocked(%s) = %v,%v want %v", s, never, private, want)
		}
	}
}

func proxyFor(t *testing.T, allow []string) *httptest.Server {
	return proxyWith(t, policy.EgressRules{Mode: policy.NetAllowlist, Allow: allow}, nil)
}

func proxyWith(t *testing.T, rules policy.EgressRules, onDecision func(Decision)) *httptest.Server {
	t.Helper()
	p := &Proxy{Lookup: func(_ context.Context, ip string) (string, policy.EgressRules, bool) {
		if ip == "127.0.0.1" {
			return "S1", rules, true
		}
		return "", policy.EgressRules{}, false
	}, OnDecision: onDecision}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv
}

func get(t *testing.T, proxy, target string) (int, string) {
	t.Helper()
	c, err := net.Dial("tcp", strings.TrimPrefix(proxy, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_, _ = c.Write([]byte("GET " + target + " HTTP/1.1\r\nHost: x\r\nConnection: close\r\n\r\n"))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b := new(strings.Builder)
	_, _ = bufio.NewReader(resp.Body).WriteTo(b)
	return resp.StatusCode, b.String()
}

func TestProxyDecisions(t *testing.T) {
	// The test upstream listens on loopback, which is never allowed — so the
	// allowed-path check uses a stub resolver pointing a name at it is not
	// possible either; assert the denials here, and the happy path in e2e.
	srv := proxyFor(t, []string{"example.test", "169.254.169.254", "127.0.0.1"})
	if code, body := get(t, srv.URL, "http://not-allowed.test/"); code != 403 || !strings.Contains(body, "allow list") {
		t.Fatalf("not allowlisted: %d %s", code, body)
	}
	if code, body := get(t, srv.URL, "http://169.254.169.254/latest/meta-data"); code != 403 || !strings.Contains(body, "blocked") {
		t.Fatalf("metadata endpoint: %d %s", code, body)
	}
	if code, body := get(t, srv.URL, "http://127.0.0.1:9/"); code != 403 || !strings.Contains(body, "blocked") {
		t.Fatalf("loopback: %d %s", code, body)
	}
	if code, _ := get(t, srv.URL, "/relative"); code != 403 {
		t.Fatalf("relative URL: %d", code)
	}
	empty := proxyFor(t, nil)
	if code, _ := get(t, empty.URL, "http://example.test/"); code != 403 {
		t.Fatalf("empty allowlist must deny: %d", code)
	}
}

func TestProxyUnknownSource(t *testing.T) {
	p := &Proxy{Lookup: func(context.Context, string) (string, policy.EgressRules, bool) {
		return "", policy.EgressRules{}, false
	}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://example.test/", nil)
	p.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("unknown source: %d", rec.Code)
	}
}

func TestProxyModes(t *testing.T) {
	var got []Decision
	rec := func(d Decision) { got = append(got, d) }

	none := proxyWith(t, policy.EgressRules{Mode: policy.NetNone, Allow: []string{"example.test"}}, rec)
	if code, body := get(t, none.URL, "http://example.test/"); code != 403 || !strings.Contains(body, "mode none") {
		t.Fatalf("mode none: %d %s", code, body)
	}

	open := proxyWith(t, policy.EgressRules{Mode: policy.NetOpen, Deny: []string{"blocked.test", "10.0.0.0/8"}}, rec)
	if code, body := get(t, open.URL, "http://api.blocked.test/"); code != 403 || !strings.Contains(body, "block list") {
		t.Fatalf("open + denied host: %d %s", code, body)
	}
	if code, body := get(t, open.URL, "http://10.1.2.3/"); code != 403 || !strings.Contains(body, "block list") {
		t.Fatalf("open + denied CIDR: %d %s", code, body)
	}
	// Open still never reaches metadata / loopback.
	if code, body := get(t, open.URL, "http://169.254.169.254/"); code != 403 || !strings.Contains(body, "blocked") {
		t.Fatalf("open + metadata: %d %s", code, body)
	}
	// Open does not reach private addresses unless the allow list names them.
	if code, body := get(t, open.URL, "http://192.168.1.1:9/"); code != 403 || !strings.Contains(body, "blocked") {
		t.Fatalf("open + private: %d %s", code, body)
	}

	if len(got) != 5 {
		t.Fatalf("decisions recorded = %d", len(got))
	}
	d := got[1]
	if d.SandboxID != "S1" || d.Host != "api.blocked.test" || d.Port != "80" || d.Method != "GET" || d.Allowed {
		t.Fatalf("decision = %+v", d)
	}
}
