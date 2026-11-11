// Package config reads the Portal's configuration from the
// environment (PORTAL_PLAN 6/S0: "cmd/portal: đọc config từ env").
// Every knob is a PORTAL_* variable with a documented default; only
// PORTAL_DATABASE_URL is required.
package config

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Config is the resolved process configuration.
type Config struct {
	// Addr is the HTTP listen address, PORTAL_ADDR (default ":8080").
	Addr string
	// DatabaseURL is the Postgres connection string,
	// PORTAL_DATABASE_URL (required; the process refuses to start
	// without it).
	DatabaseURL string
	// Debug turns on debug logging, PORTAL_DEBUG.
	Debug bool

	// PublicURL is the externally reachable base URL of the BFF /
	// API (PORTAL_PUBLIC_URL, e.g. https://app.example.com). S1 uses
	// it for the OIDC redirect and secure cookie attributes.
	PublicURL string
	// SessionTTL is the web-session lifetime, PORTAL_SESSION_TTL
	// (default 12h).
	SessionTTL time.Duration

	// OIDC settings (S1 login). Empty values simply leave the
	// login route unavailable in S0.
	OIDCIssuer       string // PORTAL_OIDC_ISSUER
	OIDCClientID     string // PORTAL_OIDC_CLIENT_ID
	OIDCClientSecret string // PORTAL_OIDC_CLIENT_SECRET

	// DBRole is the non-owner role pooled connections assume (SET
	// ROLE) so RLS applies even when the login user is the table
	// owner. PORTAL_DB_ROLE, default "portal_app"; set to "-" (or
	// "") to disable.
	DBRole string

	// Bootstrap password login (owner decision 2026-11-11: local
	// admin so the dashboard is reachable without an IdP).
	AdminUsername string // PORTAL_ADMIN_USERNAME, default "admin"
	// AdminPassword is the password for the bootstrap login,
	// PORTAL_ADMIN_PASSWORD. There is no default: unset (or "-")
	// disables the password login (SSO-only, PORTAL_PLAN 1.4). When
	// set it must be at least MinAdminPasswordLen characters. The
	// stored hash converges to this value on every boot; that is the
	// rotation mechanism.
	AdminPassword string
	// AdminEmail is the admin user's directory email,
	// PORTAL_ADMIN_EMAIL (default "admin@portal.local").
	AdminEmail string
	// PasswordLogin reports whether the local login endpoint is
	// served. Derived from AdminPassword != "-".
	PasswordLogin bool

	// Platform LLM connection for the Chat page (AGENT_PLAN 5.2.1:
	// WebHub glm-4.6 is the free default). The chat routes answer
	// 503 chat_not_configured until an API key is set; A2 replaces
	// this with per-org model connections.
	LLMBaseURL string // PORTAL_DEFAULT_LLM_BASE_URL
	LLMModel   string // PORTAL_DEFAULT_LLM_MODEL, default glm-4.6
	LLMFlavor  string // PORTAL_DEFAULT_LLM_FLAVOR: webhub | generic
	LLMAPIKey  string // PORTAL_DEFAULT_LLM_API_KEY (secret; never logged)
	// SandboxdURL is the base URL of the sandboxd control plane the
	// gateway proxies to (PORTAL_SANDBOXD_URL, e.g.
	// http://sandboxd:9191). Empty (with SandboxdAPIKey) disables the
	// whole S3 surface: /v1/sandboxes* answers 503 and the dashboard
	// Sandboxes page reports "not connected" instead of listing.
	SandboxdURL string
	// SandboxdAPIKey is the sandboxd API key the gateway calls with
	// (PORTAL_SANDBOXD_API_KEY). In production this key carries the
	// require_external flag (P3a) so an untagged create is impossible;
	// a dev sandboxd built without its auth store ignores the header.
	SandboxdAPIKey string
	// SandboxdWebhookSecret verifies X-Sandboxd-Signature on the
	// lifecycle webhook (PORTAL_SANDBOXD_WEBHOOK_SECRET). Empty
	// disables the webhook receiver (404).
	SandboxdWebhookSecret string
	// SandboxdTimeout bounds one gateway call to sandboxd,
	// PORTAL_SANDBOXD_TIMEOUT (default 30s; the streaming proxies and
	// long execs are exempt — they honor the request context).
	SandboxdTimeout time.Duration
}

// DefaultLLMBaseURL is the WebHub endpoint chosen as the free default
// (AGENT_PLAN 5.2.1).
const DefaultLLMBaseURL = "https://h11hp9dd1y90-d.space-z.ai/v1"

// ChatEnabled reports whether the platform LLM connection is usable.
func (c Config) ChatEnabled() bool { return c.LLMBaseURL != "" && c.LLMAPIKey != "" }

// MinAdminPasswordLen is the shortest PORTAL_ADMIN_PASSWORD accepted.
const MinAdminPasswordLen = 12

// Parse reads the environment (from getenv, swapped in tests) and
// returns the resolved config, validating what it can up front.
func Parse(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:             firstNonEmpty(getenv("PORTAL_ADDR"), ":8080"),
		DatabaseURL:      strings.TrimSpace(getenv("PORTAL_DATABASE_URL")),
		Debug:            isTrue(getenv("PORTAL_DEBUG")),
		PublicURL:        strings.TrimRight(strings.TrimSpace(getenv("PORTAL_PUBLIC_URL")), "/"),
		OIDCIssuer:       strings.TrimSpace(getenv("PORTAL_OIDC_ISSUER")),
		OIDCClientID:     strings.TrimSpace(getenv("PORTAL_OIDC_CLIENT_ID")),
		OIDCClientSecret: getenv("PORTAL_OIDC_CLIENT_SECRET"),
		SessionTTL:       12 * time.Hour,
	}
	// "-" explicitly disables the SET ROLE switch (local psql-style
	// debugging); the default is the portal_app role.
	switch role := strings.TrimSpace(getenv("PORTAL_DB_ROLE")); role {
	case "":
		cfg.DBRole = "portal_app"
	case "-":
		cfg.DBRole = ""
	default:
		cfg.DBRole = role
	}
	// Bootstrap password login. Off unless PORTAL_ADMIN_PASSWORD is
	// set: a built-in default password on a public deployment is an
	// open door (it was admin/admin123 before P0).
	cfg.AdminUsername = firstNonEmpty(strings.TrimSpace(getenv("PORTAL_ADMIN_USERNAME")), "admin")
	cfg.AdminEmail = firstNonEmpty(strings.TrimSpace(getenv("PORTAL_ADMIN_EMAIL")), "admin@portal.local")
	switch pw := getenv("PORTAL_ADMIN_PASSWORD"); pw {
	case "", "-":
		cfg.AdminPassword, cfg.PasswordLogin = "", false
	default:
		if len(pw) < MinAdminPasswordLen {
			return cfg, fmt.Errorf("PORTAL_ADMIN_PASSWORD must be at least %d characters (or unset to disable the password login)", MinAdminPasswordLen)
		}
		cfg.AdminPassword, cfg.PasswordLogin = pw, true
	}
	cfg.LLMBaseURL = strings.TrimRight(firstNonEmpty(strings.TrimSpace(getenv("PORTAL_DEFAULT_LLM_BASE_URL")), DefaultLLMBaseURL), "/")
	cfg.LLMModel = firstNonEmpty(strings.TrimSpace(getenv("PORTAL_DEFAULT_LLM_MODEL")), "glm-4.6")
	cfg.LLMAPIKey = strings.TrimSpace(getenv("PORTAL_DEFAULT_LLM_API_KEY"))
	switch f := strings.ToLower(strings.TrimSpace(getenv("PORTAL_DEFAULT_LLM_FLAVOR"))); f {
	case "", "webhub":
		cfg.LLMFlavor = "webhub"
	case "generic":
		cfg.LLMFlavor = "generic"
	default:
		return cfg, fmt.Errorf("PORTAL_DEFAULT_LLM_FLAVOR: want webhub or generic, got %q", f)
	}
	if !strings.HasPrefix(cfg.LLMBaseURL, "https://") && !strings.HasPrefix(cfg.LLMBaseURL, "http://") {
		return cfg, fmt.Errorf("PORTAL_DEFAULT_LLM_BASE_URL must be an http(s) URL")
	}
	if v := getenv("PORTAL_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("PORTAL_SESSION_TTL: invalid duration %q", v)
		}
		cfg.SessionTTL = d
	}
	cfg.SandboxdURL = strings.TrimRight(strings.TrimSpace(getenv("PORTAL_SANDBOXD_URL")), "/")
	cfg.SandboxdAPIKey = strings.TrimSpace(getenv("PORTAL_SANDBOXD_API_KEY"))
	cfg.SandboxdWebhookSecret = getenv("PORTAL_SANDBOXD_WEBHOOK_SECRET")
	cfg.SandboxdTimeout = 30 * time.Second
	if v := getenv("PORTAL_SANDBOXD_TIMEOUT"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("PORTAL_SANDBOXD_TIMEOUT: invalid duration %q", v)
		}
		cfg.SandboxdTimeout = d
	}
	if cfg.DatabaseURL == "" {
		return cfg, errors.New("PORTAL_DATABASE_URL is required (postgres://user:pass@host:5432/db)")
	}
	return cfg, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func isTrue(v string) bool {
	b, err := strconv.ParseBool(v)
	return err == nil && b
}
