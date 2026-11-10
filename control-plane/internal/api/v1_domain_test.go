package api

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/domain"
)

type stubDNS map[string][]string

func (f stubDNS) LookupHost(_ context.Context, h string) ([]string, error) {
	if strings.HasPrefix(h, "sandboxd-verify-") {
		h = "*" + h[strings.Index(h, "."):]
	}
	if a, ok := f[h]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestDomainEndpoints(t *testing.T) {
	s, _ := newSandboxRowServer(t)
	s.PreviewDomain = "localhost"
	s.Certs = &domain.CertStore{Dir: t.TempDir()}
	s.DomainResolver = stubDNS{"console.example.com": {"203.0.113.5"}, "*.preview.example.com": {"203.0.113.5"}}

	w := callAs(s.v1GetDomain, "user", "GET", "/v1/domain", "", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"restart_required":false`) {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1VerifyDomain, "user", "POST", "/", "", nil, nil); w.Code != 400 {
		t.Fatalf("verify localhost: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1PutDomain, "user", "PUT", "/", `{"domain":"bad_domain"}`, nil, nil); w.Code != 400 {
		t.Fatalf("bad domain: %d", w.Code)
	}
	w = callAs(s.v1PutDomain, "user", "PUT", "/", `{"domain":"Example.com","tls_mode":"custom"}`, nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"domain":"example.com"`) || !strings.Contains(w.Body.String(), `"restart_required":true`) {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	w = callAs(s.v1VerifyDomain, "user", "POST", "/", `{"expect_ips":["203.0.113.5"]}`, nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"ok":true`) {
		t.Fatalf("verify: %d %s", w.Code, w.Body)
	}
	if w := callAs(s.v1AddCertificate, "user", "POST", "/", `{"cert_pem":"x","key_pem":"y"}`, nil, nil); w.Code != 400 {
		t.Fatalf("garbage cert: %d %s", w.Code, w.Body)
	}
	w = callAs(s.v1ListCertificates, "user", "GET", "/", "", nil, nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"certificates":[]`) {
		t.Fatalf("list: %d %s", w.Code, w.Body)
	}
	if c, ok, err := StoredDomain(s.Store, context.Background()); err != nil || !ok || c.TLSMode != "custom" {
		t.Fatalf("stored: %+v %v %v", c, ok, err)
	}
}
