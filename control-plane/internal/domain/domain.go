// Package domain is the instance's domain manager (PLAN §4 "Domain"): which
// domain previews and the console live under, how TLS is terminated, a DNS
// check that the wildcard points somewhere, and custom wildcard certificates
// handed to Traefik through its file provider.
//
// Domain and TLS mode are stored settings applied at the next start: every
// preview router label and URL derives from them, so changing them live
// would leave running containers with stale routes. GET /v1/domain reports
// when the stored value differs from the running one.
package domain

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"regexp"
	"strings"
)

// TLS modes.
const (
	TLSOff         = "off"         // plain HTTP (default)
	TLSCustom      = "custom"      // a wildcard cert uploaded via POST /v1/domain/certificates
	TLSLetsEncrypt = "letsencrypt" // Traefik's ACME resolver (configured in traefik.yml)
)

// Config is the stored domain setting (instance_config key "domain").
type Config struct {
	Domain    string `json:"domain"`
	TLSMode   string `json:"tls_mode"`
	ACMEEmail string `json:"acme_email,omitempty"`
}

var labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidHost reports whether h is a lower-case DNS name (no trailing dot).
func ValidHost(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if !labelRE.MatchString(l) {
			return false
		}
	}
	return true
}

// Normalize lower-cases and defaults the config and validates it.
func (c *Config) Normalize() error {
	c.Domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(c.Domain)), ".")
	if c.TLSMode == "" {
		c.TLSMode = TLSOff
	}
	if !ValidHost(c.Domain) {
		return fmt.Errorf("domain %q is not a valid DNS name", c.Domain)
	}
	switch c.TLSMode {
	case TLSOff, TLSCustom:
	case TLSLetsEncrypt:
		if c.Domain == "localhost" || strings.HasSuffix(c.Domain, ".localhost") {
			return errors.New("letsencrypt cannot issue certificates for localhost")
		}
		if c.ACMEEmail != "" {
			if _, err := mail.ParseAddress(c.ACMEEmail); err != nil {
				return fmt.Errorf("acme_email: %v", err)
			}
		}
	default:
		return fmt.Errorf("tls_mode must be %s, %s or %s", TLSOff, TLSCustom, TLSLetsEncrypt)
	}
	return nil
}

// TLS reports whether previews are served over HTTPS in this mode.
func (c Config) TLS() bool { return c.TLSMode == TLSCustom || c.TLSMode == TLSLetsEncrypt }

// Check is one DNS lookup in a verification.
type Check struct {
	Host  string   `json:"host"`
	Addrs []string `json:"addrs"`
	OK    bool     `json:"ok"`
	Error string   `json:"error,omitempty"`
}

// Verification is the result of POST /v1/domain/verify.
type Verification struct {
	Domain string  `json:"domain"`
	OK     bool    `json:"ok"`
	Checks []Check `json:"checks"`
	Hint   string  `json:"hint,omitempty"`
}

// Resolver is the subset of net.Resolver Verify uses.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// Verify checks that the console host and a random preview host (proving the
// wildcard record) resolve, and — when expect is non-empty — to one of those
// addresses. hosts are the console host and the preview suffix (".preview.D"
// or ".D"), so both host styles are covered.
func Verify(ctx context.Context, r Resolver, d, consoleHost, previewSuffix string, expect []string) Verification {
	if r == nil {
		r = net.DefaultResolver
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	probe := "sandboxd-verify-" + hex.EncodeToString(b) + previewSuffix
	v := Verification{Domain: d, OK: true}
	for _, h := range []string{consoleHost, probe} {
		c := Check{Host: h, Addrs: []string{}}
		addrs, err := r.LookupHost(ctx, h)
		switch {
		case err != nil:
			c.Error = err.Error()
		case len(expect) > 0 && !overlaps(addrs, expect):
			c.Addrs = addrs
			c.Error = fmt.Sprintf("resolves to %v, expected one of %v", addrs, expect)
		default:
			c.Addrs, c.OK = addrs, true
		}
		if !c.OK {
			v.OK = false
		}
		v.Checks = append(v.Checks, c)
	}
	if !v.OK {
		v.Hint = fmt.Sprintf("create DNS A/AAAA records for %s and *%s pointing at this server", consoleHost, previewSuffix)
	}
	return v
}

func overlaps(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}
