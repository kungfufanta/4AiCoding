package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/audit"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/domain"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/previewhost"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
)

// domainConfigKey is the instance_config key holding domain.Config.
const domainConfigKey = "domain"

// StoredDomain reads the stored domain config; ok=false when none is stored.
// main uses it at startup to override PREVIEW_DOMAIN / PREVIEW_TLS.
func StoredDomain(st *store.Store, ctx context.Context) (domain.Config, bool, error) {
	v, err := st.GetConfig(ctx, domainConfigKey)
	if errors.Is(err, store.ErrNotFound) {
		return domain.Config{}, false, nil
	}
	if err != nil {
		return domain.Config{}, false, err
	}
	var c domain.Config
	if err := json.Unmarshal([]byte(v), &c); err != nil {
		return domain.Config{}, false, err
	}
	return c, true, nil
}

// runningDomain is what this process actually serves.
func (s *Server) runningDomain() domain.Config {
	mode := domain.TLSOff
	if s.PreviewTLS {
		mode = s.DomainTLSMode
		if mode == "" || mode == domain.TLSOff {
			mode = domain.TLSCustom
		}
	}
	return domain.Config{Domain: s.PreviewDomain, TLSMode: mode}
}

// GET /v1/domain
func (s *Server) v1GetDomain(w http.ResponseWriter, r *http.Request) {
	running := s.runningDomain()
	stored, ok, err := StoredDomain(s.Store, r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok {
		stored = running
	}
	s.writeDomain(w, stored, running)
}

func (s *Server) writeDomain(w http.ResponseWriter, stored, running domain.Config) {
	hs := s.hosts()
	writeJSON(w, http.StatusOK, map[string]any{
		"domain":           stored.Domain,
		"tls_mode":         stored.TLSMode,
		"acme_email":       stored.ACMEEmail,
		"running":          map[string]any{"domain": running.Domain, "tls_mode": running.TLSMode},
		"restart_required": stored.Domain != running.Domain || stored.TLS() != running.TLS(),
		"console_host":     hs.Console(),
		"preview_example":  hs.Preview("<id>", 3000),
	})
}

// PUT /v1/domain {domain, tls_mode, acme_email}. Applied at the next start.
func (s *Server) v1PutDomain(w http.ResponseWriter, r *http.Request) {
	var c domain.Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	if err := c.Normalize(); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	b, _ := json.Marshal(c)
	if err := s.Store.SetConfig(r.Context(), domainConfigKey, string(b)); err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "domain.update", Target: c.Domain,
		Detail: map[string]any{"tls_mode": c.TLSMode}})
	s.writeDomain(w, c, s.runningDomain())
}

// POST /v1/domain/verify {expect_ips?: [...]} — resolve the console host and
// a random preview host under the stored (or running) domain.
func (s *Server) v1VerifyDomain(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ExpectIPs []string `json:"expect_ips"`
	}
	if r.ContentLength > 0 {
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4*1024))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
			return
		}
	}
	c, ok, err := StoredDomain(s.Store, r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok {
		c = s.runningDomain()
	}
	hs := s.hosts()
	hs.Domain = c.Domain
	if c.Domain == "localhost" || strings.HasSuffix(c.Domain, ".localhost") {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "localhost needs no DNS: browsers resolve *.localhost to 127.0.0.1")
		return
	}
	v := domain.Verify(r.Context(), s.DomainResolver, c.Domain, hs.Console(), previewSuffix(hs), body.ExpectIPs)
	writeJSON(w, http.StatusOK, v)
}

func previewSuffix(hs previewhost.Scheme) string { return hs.Suffix() }

// GET /v1/domain/certificates
func (s *Server) v1ListCertificates(w http.ResponseWriter, r *http.Request) {
	list, err := s.Certs.List()
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"certificates": list, "enabled": s.Certs.Enabled()})
}

// POST /v1/domain/certificates {cert_pem, key_pem} — install a custom
// wildcard certificate covering the console and preview hosts.
func (s *Server) v1AddCertificate(w http.ResponseWriter, r *http.Request) {
	if !s.Certs.Enabled() {
		writeV1Err(w, http.StatusServiceUnavailable, "not_configured", "certificate storage is not configured (SANDBOXD_TRAEFIK_DYNAMIC_DIR)")
		return
	}
	var body struct {
		CertPEM string `json:"cert_pem"`
		KeyPEM  string `json:"key_pem"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256*1024))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", "invalid json: "+err.Error())
		return
	}
	c, ok, err := StoredDomain(s.Store, r.Context())
	if err != nil {
		writeV1Err(w, http.StatusInternalServerError, "internal", err.Error())
		return
	}
	if !ok {
		c = s.runningDomain()
	}
	hs := s.hosts()
	hs.Domain = c.Domain
	want := []string{hs.Console(), hs.Preview("x", 3000)}
	cert, err := s.Certs.Add([]byte(body.CertPEM), []byte(body.KeyPEM), want, time.Now())
	if err != nil {
		writeV1Err(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	s.auditAction(r, audit.Entry{Action: "domain.certificate", Target: cert.ID,
		Detail: map[string]any{"dns_names": cert.DNSNames, "not_after": cert.NotAfter}})
	writeJSON(w, http.StatusCreated, cert)
}
