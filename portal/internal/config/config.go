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
}

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
	if v := getenv("PORTAL_SESSION_TTL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("PORTAL_SESSION_TTL: invalid duration %q", v)
		}
		cfg.SessionTTL = d
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
