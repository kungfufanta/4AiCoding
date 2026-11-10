package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

func TestPreviewURLFlatStyle(t *testing.T) {
	cases := []struct {
		style, tag, want string
	}{
		{"", "", "http://s-01ABC-3000.preview.ex.com"},
		{"nested", "eu1", "http://s-01ABC-3000.preview.ex.com"},
		{"flat", "", "http://s-01ABC-3000.ex.com"},
		{"flat", "eu1", "http://s-01ABC-3000--eu1.ex.com"},
	}
	for _, c := range cases {
		s := &Server{PreviewDomain: "ex.com", PreviewHostStyle: c.style, PreviewHostTag: c.tag}
		if got := s.previewURL("01ABC", 3000); got != c.want {
			t.Errorf("style=%q tag=%q: previewURL = %q; want %q", c.style, c.tag, got, c.want)
		}
	}
	s := &Server{PreviewDomain: "ex.com", PreviewHostStyle: "flat", PreviewHostTag: "eu1", PublicHTTPPort: "18080"}
	if got := s.previewBase(); got != "http://s-*-*--eu1.ex.com:18080" {
		t.Errorf("previewBase = %q", got)
	}
	if got := (&Server{PreviewDomain: "ex.com"}).previewBase(); got != "http://*.preview.ex.com" {
		t.Errorf("nested previewBase = %q", got)
	}
}

func TestSettingsReportHostStyle(t *testing.T) {
	s := &Server{PreviewDomain: "ex.com", PreviewHostStyle: "flat", PreviewHostTag: "eu1", Live: nil}
	w := httptest.NewRecorder()
	s.v1GetSettings(w, httptest.NewRequest("GET", "/v1/settings", nil))
	var m struct {
		Networking struct {
			Style string `json:"preview_host_style"`
			Tag   string `json:"preview_host_tag"`
			Base  string `json:"preview_base"`
		} `json:"networking"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("%v: %s", err, w.Body)
	}
	if m.Networking.Style != "flat" || m.Networking.Tag != "eu1" || m.Networking.Base != "http://s-*-*--eu1.ex.com" {
		t.Errorf("networking = %+v", m.Networking)
	}
}

func TestValidateReturnURLFlat(t *testing.T) {
	hs := (&Server{PreviewDomain: "ex.com", PreviewHostStyle: "flat", PreviewHostTag: "eu1"}).hosts()
	ok := []string{"https://s-SB1-3000--eu1.ex.com/", "https://s-SB1-3000--eu1.ex.com", "https://api--eu1.ex.com/x"}
	bad := []string{"https://s-SB1-3000.ex.com/", "https://s-SB1-3000--eu2.ex.com/", "https://s-SB1-3000.preview.ex.com/",
		"https://api.ex.com/", "https://api.preview.ex.com/", "https://s-SB2-3000--eu1.ex.com/", "https://console--eu1.ex.com/"}
	for _, u := range ok {
		if !validateReturnURL(u, "SB1", hs) {
			t.Errorf("%s: want allowed", u)
		}
	}
	for _, u := range bad {
		if validateReturnURL(u, "SB1", hs) {
			t.Errorf("%s: want rejected", u)
		}
	}
}

func signPreviewToken(t *testing.T, secret, sandboxID string, exp time.Time) string {
	t.Helper()
	enc := base64.RawURLEncoding
	hb, _ := json.Marshal(map[string]any{"alg": "HS256", "typ": "JWT", "kid": "k1"})
	cb, _ := json.Marshal(map[string]any{
		"iss": "upstream", "iat": time.Now().Add(-time.Minute).Unix(), "exp": exp.Unix(),
		"aud": auth.PreviewAudience, "sub": "user-1", "sandbox_id": sandboxID,
	})
	in := enc.EncodeToString(hb) + "." + enc.EncodeToString(cb)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(in))
	return in + "." + enc.EncodeToString(mac.Sum(nil))
}

// cookieDomains collects the Domain attribute of every Set-Cookie header.
func cookieDomains(w *httptest.ResponseRecorder) []string {
	var out []string
	for _, c := range w.Result().Cookies() {
		out = append(out, c.Domain)
	}
	return out
}

// In the flat host style the only parent shared by all preview hosts is
// `.<domain>` — which other instances (tags), the console and the API sit
// under too. The preview session cookie must therefore never carry a Domain
// attribute: /preview-auth hands the browser to the preview host's own
// landing path, and /forward-auth answers that with a host-only cookie.
func TestFlatPreviewAuthCookieIsHostOnly(t *testing.T) {
	st, err := store.Open(context.Background(), "file::memory:?_fk=1", "../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	const sbID = "01SBFLAT0000000000000001"
	if err := st.Create(context.Background(), &store.Sandbox{ID: sbID, Status: "running", Visibility: "private"}); err != nil {
		t.Fatal(err)
	}
	const secret = "s3cret"
	s := &Server{Store: st, PreviewDomain: "ex.com", PreviewHostStyle: "flat", PreviewHostTag: "eu1"}
	s.Auth = auth.NewMiddleware(&auth.Config{
		PreviewSecrets:  map[string]string{"k1": secret},
		AuthRedirectURL: "https://login.example/auth?sandbox_id={sandbox_id}&return={return}",
	}, NewStoreResolver(st), nil, nil)

	previewHost := "s-" + sbID + "-3000--eu1.ex.com"
	returnURL := "https://" + previewHost + "/app?x=1"
	tok := signPreviewToken(t, secret, sbID, time.Now().Add(time.Hour))

	// Step 1: /preview-auth on the API host sets nothing and bounces to
	// the preview host's landing path.
	w := httptest.NewRecorder()
	s.handlePreviewAuth(w, httptest.NewRequest("GET", "/preview-auth?"+url.Values{"token": {tok}, "return": {returnURL}}.Encode(), nil))
	if w.Code != http.StatusFound {
		t.Fatalf("preview-auth: %d %s", w.Code, w.Body)
	}
	if got := cookieDomains(w); len(got) != 0 {
		t.Fatalf("preview-auth set cookies with domains %q; want none", got)
	}
	loc, err := url.Parse(w.Header().Get("Location"))
	if err != nil || loc.Scheme != "https" || loc.Host != previewHost || loc.Path != flatAuthLandingPath {
		t.Fatalf("preview-auth Location = %q", w.Header().Get("Location"))
	}

	// Step 2: Traefik forward-auths the landing request on the preview host.
	fwd := func(host, uri string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/forward-auth", nil)
		r.Header.Set("X-Forwarded-Host", host)
		r.Header.Set("X-Forwarded-Uri", uri)
		s.handleForwardAuth(w, r)
		return w
	}
	w = fwd(previewHost, loc.RequestURI())
	if w.Code != http.StatusFound || w.Header().Get("Location") != returnURL {
		t.Fatalf("landing: %d Location=%q body=%s", w.Code, w.Header().Get("Location"), w.Body)
	}
	cs := w.Result().Cookies()
	if len(cs) != 1 || cs[0].Name != previewCookieName || cs[0].Value != tok {
		t.Fatalf("landing cookies = %+v", cs)
	}
	for _, c := range cs {
		if c.Domain != "" {
			t.Fatalf("flat-mode cookie has Domain=%q; must be host-only", c.Domain)
		}
	}
	if raw := w.Header().Get("Set-Cookie"); strings.Contains(strings.ToLower(raw), "domain=") {
		t.Fatalf("raw Set-Cookie carries a Domain attribute: %s", raw)
	}

	// The cookie then satisfies forward-auth for that host.
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/forward-auth", nil)
	r.Header.Set("X-Forwarded-Host", previewHost)
	r.Header.Set("X-Forwarded-Uri", "/app")
	r.AddCookie(&http.Cookie{Name: previewCookieName, Value: tok})
	s.handleForwardAuth(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("forward-auth with cookie: %d", w.Code)
	}

	// Landing on a different host than the return URL names sets nothing:
	// the cookie can never be planted for a host other than the one served.
	other := "s-" + sbID + "-4000--eu1.ex.com"
	w = fwd(other, loc.RequestURI())
	if w.Code != http.StatusBadRequest || len(w.Result().Cookies()) != 0 {
		t.Fatalf("cross-host landing: %d cookies=%d", w.Code, len(w.Result().Cookies()))
	}
	// A token for another sandbox is refused too.
	badTok := signPreviewToken(t, secret, "01OTHER", time.Now().Add(time.Hour))
	w = fwd(previewHost, flatAuthLandingPath+"?"+url.Values{"token": {badTok}, "return": {returnURL}}.Encode())
	if w.Code == http.StatusOK || len(w.Result().Cookies()) != 0 {
		t.Fatalf("foreign token: %d cookies=%d", w.Code, len(w.Result().Cookies()))
	}
}

// Nested mode keeps the shared `.preview.<domain>` cookie.
func TestNestedPreviewAuthCookieDomain(t *testing.T) {
	const secret = "s3cret"
	s := &Server{PreviewDomain: "ex.com"}
	s.Auth = auth.NewMiddleware(&auth.Config{PreviewSecrets: map[string]string{"k1": secret}}, nil, nil, nil)
	tok := signPreviewToken(t, secret, "SB1", time.Now().Add(time.Hour))
	w := httptest.NewRecorder()
	s.handlePreviewAuth(w, httptest.NewRequest("GET", "/preview-auth?"+url.Values{
		"token": {tok}, "return": {"https://s-SB1-3000.preview.ex.com/"}}.Encode(), nil))
	if w.Code != http.StatusFound {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got := cookieDomains(w); len(got) != 1 || got[0] != "preview.ex.com" {
		t.Fatalf("cookie domains = %q", got)
	}
}
