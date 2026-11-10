package config

import (
	"testing"
	"time"
)

func TestParseDefaults(t *testing.T) {
	env := func(k string) string {
		if k == "PORTAL_DATABASE_URL" {
			return "postgres://u:p@localhost:5432/portal"
		}
		return ""
	}
	cfg, err := Parse(env)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.SessionTTL != 12*time.Hour {
		t.Errorf("SessionTTL = %v, want 12h", cfg.SessionTTL)
	}
	if cfg.Debug {
		t.Error("Debug = true, want false")
	}
}

func TestParseRequiresDatabaseURL(t *testing.T) {
	if _, err := Parse(func(string) string { return "" }); err == nil {
		t.Fatal("Parse without PORTAL_DATABASE_URL: want error, got nil")
	}
}

func TestParseSessionTTL(t *testing.T) {
	env := func(k string) string {
		switch k {
		case "PORTAL_DATABASE_URL":
			return "postgres://u:p@localhost:5432/portal"
		case "PORTAL_SESSION_TTL":
			return "45m"
		default:
			return ""
		}
	}
	cfg, err := Parse(env)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.SessionTTL != 45*time.Minute {
		t.Errorf("SessionTTL = %v, want 45m", cfg.SessionTTL)
	}
}

func TestParseBadSessionTTL(t *testing.T) {
	env := func(k string) string {
		switch k {
		case "PORTAL_DATABASE_URL":
			return "postgres://u:p@localhost:5432/portal"
		case "PORTAL_SESSION_TTL":
			return "soon"
		default:
			return ""
		}
	}
	if _, err := Parse(env); err == nil {
		t.Fatal("Parse with PORTAL_SESSION_TTL=soon: want error, got nil")
	}
}

func TestParsePublicURLTrimmed(t *testing.T) {
	env := func(k string) string {
		switch k {
		case "PORTAL_DATABASE_URL":
			return "postgres://u:p@localhost:5432/portal"
		case "PORTAL_PUBLIC_URL":
			return "https://app.example.com/"
		default:
			return ""
		}
	}
	cfg, err := Parse(env)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.PublicURL != "https://app.example.com" {
		t.Errorf("PublicURL = %q, want trailing slash trimmed", cfg.PublicURL)
	}
}
