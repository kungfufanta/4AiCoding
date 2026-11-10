package api

import (
	"regexp"
	"strings"
	"sync"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/auth"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/previewhost"
)

// authCfg returns the current auth config snapshot, or an empty config
// when auth is not wired (nil-safe for tests / partial wiring).
func (s *Server) authCfg() *auth.Config {
	if s.Auth != nil {
		if c := s.Auth.Snapshot(); c != nil {
			return c
		}
	}
	return &auth.Config{}
}

// hosts returns the hostname scheme this instance serves under. An
// unset style is nested (the default layout).
func (s *Server) hosts() previewhost.Scheme {
	style, _ := previewhost.ParseStyle(s.PreviewHostStyle)
	tag := s.PreviewHostTag
	if style != previewhost.StyleFlat {
		tag = ""
	}
	return previewhost.Scheme{Domain: s.PreviewDomain, Style: style, Tag: tag}
}

// reCache memoizes the per-scheme regexes so /forward-auth (called on
// every request to a private sandbox) does not recompile them.
var reCache sync.Map // key -> *regexp.Regexp

func cachedRE(key, pattern string) *regexp.Regexp {
	if v, ok := reCache.Load(key); ok {
		return v.(*regexp.Regexp)
	}
	re := regexp.MustCompile(pattern)
	reCache.Store(key, re)
	return re
}

// parseSandboxIDFromHost extracts the sandbox id from a preview host
// (optional :port), or "" if the host does not match the preview shape
// of hs.
func parseSandboxIDFromHost(host string, hs previewhost.Scheme) string {
	re := cachedRE("host|"+hs.Suffix(), hs.HostRegexp().String())
	m := re.FindStringSubmatch(strings.TrimSpace(host))
	if m == nil {
		return ""
	}
	return m[1]
}

// returnSandboxRE matches an allowed `s-<id>-<port>` preview return URL
// and captures the id.
func returnSandboxRE(hs previewhost.Scheme) *regexp.Regexp {
	return cachedRE("ret-sb|"+hs.Suffix(),
		`^https://s-([0-9A-Za-z]+)-[0-9]+`+regexp.QuoteMeta(hs.Suffix())+`(/.*)?$`)
}

// validateReturnURL enforces two rules: no open redirects (the
// URL must be a preview host or the API host), and an
// `s-<id>` return host must carry the same sandbox_id as the JWT.
func validateReturnURL(returnURL, jwtSandboxID string, hs previewhost.Scheme) bool {
	if m := returnSandboxRE(hs).FindStringSubmatch(returnURL); m != nil {
		return m[1] == jwtSandboxID
	}
	apiRE := cachedRE("ret-api|"+hs.API(),
		`^https://`+regexp.QuoteMeta(hs.API())+`(/.*)?$`)
	return apiRE.MatchString(returnURL)
}

// parseReturnSandboxID best-effort extracts a sandbox id from a return
// URL, for the redirect-on-failure path (sandbox_id "if discoverable").
func parseReturnSandboxID(returnURL string, hs previewhost.Scheme) string {
	if m := returnSandboxRE(hs).FindStringSubmatch(returnURL); m != nil {
		return m[1]
	}
	return ""
}
