package domain

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	c := Config{Domain: " Example.COM. "}
	if err := c.Normalize(); err != nil || c.Domain != "example.com" || c.TLSMode != TLSOff {
		t.Fatalf("%+v %v", c, err)
	}
	for _, bad := range []Config{
		{Domain: ""}, {Domain: "a_b.com"}, {Domain: "-a.com"}, {Domain: "a..com"},
		{Domain: "example.com", TLSMode: "acme"},
		{Domain: "localhost", TLSMode: TLSLetsEncrypt},
		{Domain: "example.com", TLSMode: TLSLetsEncrypt, ACMEEmail: "not an email"},
	} {
		if err := bad.Normalize(); err == nil {
			t.Errorf("%+v must be rejected", bad)
		}
	}
}

type fakeResolver map[string][]string

func (f fakeResolver) LookupHost(_ context.Context, h string) ([]string, error) {
	if strings.HasPrefix(h, "sandboxd-verify-") {
		h = "*" + h[strings.Index(h, "."):]
	}
	if a, ok := f[h]; ok {
		return a, nil
	}
	return nil, errors.New("no such host")
}

func TestVerify(t *testing.T) {
	r := fakeResolver{"console.example.com": {"203.0.113.5"}, "*.preview.example.com": {"203.0.113.5"}}
	v := Verify(context.Background(), r, "example.com", "console.example.com", ".preview.example.com", nil)
	if !v.OK || len(v.Checks) != 2 {
		t.Fatalf("%+v", v)
	}
	v = Verify(context.Background(), r, "example.com", "console.example.com", ".preview.example.com", []string{"198.51.100.1"})
	if v.OK || v.Hint == "" {
		t.Fatalf("wrong address must fail: %+v", v)
	}
	v = Verify(context.Background(), fakeResolver{}, "example.com", "console.example.com", ".preview.example.com", nil)
	if v.OK {
		t.Fatal("missing records must fail")
	}
}

func selfSigned(t *testing.T, names []string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: names[0]},
		DNSNames: names, NotBefore: time.Now().Add(-time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb})
}

func TestCertStore(t *testing.T) {
	dir := t.TempDir()
	cs := &CertStore{Dir: dir, TraefikDir: "/etc/traefik/dynamic"}
	want := []string{"console.example.com", "x.preview.example.com"}
	cp, kp := selfSigned(t, []string{"console.example.com", "*.preview.example.com"}, time.Now().Add(90*24*time.Hour))
	c, err := cs.Add(cp, kp, want, time.Now())
	if err != nil || !c.Default || len(c.DNSNames) != 2 {
		t.Fatalf("add: %+v %v", c, err)
	}
	yml, _ := os.ReadFile(filepath.Join(dir, configFile))
	if !strings.Contains(string(yml), "/etc/traefik/dynamic/certs/"+c.ID+".crt") || !strings.Contains(string(yml), "defaultCertificate") {
		t.Fatalf("traefik config:\n%s", yml)
	}
	if st, _ := os.Stat(filepath.Join(dir, "certs", c.ID+".key")); st.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %v", st.Mode())
	}
	list, err := cs.List()
	if err != nil || len(list) != 1 || list[0].ID != c.ID || !list[0].Default {
		t.Fatalf("list: %+v %v", list, err)
	}

	// Wrong names, expired, mismatched key.
	cp2, kp2 := selfSigned(t, []string{"other.example"}, time.Now().Add(time.Hour))
	if _, err := cs.Add(cp2, kp2, want, time.Now()); err == nil {
		t.Error("cert for other names accepted")
	}
	cp3, kp3 := selfSigned(t, []string{"console.example.com", "*.preview.example.com"}, time.Now().Add(time.Minute))
	if _, err := cs.Add(cp3, kp3, want, time.Now().Add(time.Hour)); err == nil {
		t.Error("expired cert accepted")
	}
	if _, err := cs.Add(cp, kp2, want, time.Now()); err == nil {
		t.Error("mismatched key accepted")
	}
	if (&CertStore{}).Enabled() {
		t.Error("empty store enabled")
	}
}
