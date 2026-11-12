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

func TestParsePasswordLogin(t *testing.T) {
	base := func(kv map[string]string) func(string) string {
		return func(k string) string {
			if k == "PORTAL_DATABASE_URL" {
				return "postgres://u:p@localhost:5432/portal"
			}
			return kv[k]
		}
	}

	// Default: no built-in password, so the password login is off.
	cfg, err := Parse(base(nil))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if cfg.PasswordLogin || cfg.AdminPassword != "" || cfg.AdminUsername != "admin" {
		t.Errorf("defaults = user %q pass %q enabled %v; want admin, no password, disabled",
			cfg.AdminUsername, cfg.AdminPassword, cfg.PasswordLogin)
	}

	// Too-short passwords (the old admin123 default among them) fail
	// loudly instead of booting with a guessable login.
	for _, pw := range []string{"admin123", "s3cret", "elevenchars"} {
		if _, err := Parse(base(map[string]string{"PORTAL_ADMIN_PASSWORD": pw})); err == nil {
			t.Errorf("Parse accepted short PORTAL_ADMIN_PASSWORD %q", pw)
		}
	}
	if cfg.AdminEmail != "admin@portal.local" {
		t.Errorf("AdminEmail = %q, want admin@portal.local", cfg.AdminEmail)
	}

	// "-" disables the password login entirely (SSO-only).
	cfg, err = Parse(base(map[string]string{"PORTAL_ADMIN_PASSWORD": "-"}))
	if err != nil {
		t.Fatalf("Parse dash: %v", err)
	}
	if cfg.PasswordLogin || cfg.AdminPassword != "" {
		t.Errorf("dash password = pass %q enabled %v; want disabled", cfg.AdminPassword, cfg.PasswordLogin)
	}

	// Custom bootstrap identity.
	cfg, err = Parse(base(map[string]string{
		"PORTAL_ADMIN_USERNAME": "root",
		"PORTAL_ADMIN_PASSWORD": "s3cret-long-enough",
		"PORTAL_ADMIN_EMAIL":    "root@corp.example",
	}))
	if err != nil {
		t.Fatalf("Parse custom: %v", err)
	}
	if !cfg.PasswordLogin || cfg.AdminUsername != "root" || cfg.AdminPassword != "s3cret-long-enough" || cfg.AdminEmail != "root@corp.example" {
		t.Errorf("custom = %+v", cfg)
	}
}

func TestParseLLMDefaults(t *testing.T) {
	env := map[string]string{"PORTAL_DATABASE_URL": "postgres://x"}
	cfg, err := Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMBaseURL != DefaultLLMBaseURL || cfg.LLMModel != "glm-4.6" || cfg.LLMFlavor != "webhub" {
		t.Fatalf("defaults = %q %q %q", cfg.LLMBaseURL, cfg.LLMModel, cfg.LLMFlavor)
	}
	if cfg.ChatEnabled() {
		t.Fatal("chat must stay off without an API key")
	}
	env["PORTAL_DEFAULT_LLM_API_KEY"] = "wh_test"
	env["PORTAL_DEFAULT_LLM_BASE_URL"] = "http://llm.local/v1/"
	env["PORTAL_DEFAULT_LLM_FLAVOR"] = "generic"
	cfg, err = Parse(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.ChatEnabled() || cfg.LLMBaseURL != "http://llm.local/v1" || cfg.LLMFlavor != "generic" {
		t.Fatalf("got %+v", cfg)
	}
	env["PORTAL_DEFAULT_LLM_BASE_URL"] = "https://hub.example/"
	if cfg, err = Parse(func(k string) string { return env[k] }); err != nil || cfg.LLMBaseURL != "https://hub.example/v1" {
		t.Fatalf("bare host base url = %q, %v; want /v1 appended", cfg.LLMBaseURL, err)
	}
	env["PORTAL_DEFAULT_LLM_BASE_URL"] = "https://hub.example/api/openai"
	if cfg, err = Parse(func(k string) string { return env[k] }); err != nil || cfg.LLMBaseURL != "https://hub.example/api/openai" {
		t.Fatalf("explicit path base url = %q, %v; want unchanged", cfg.LLMBaseURL, err)
	}
	env["PORTAL_DEFAULT_LLM_FLAVOR"] = "bogus"
	if _, err := Parse(func(k string) string { return env[k] }); err == nil {
		t.Fatal("bogus flavor accepted")
	}
	env["PORTAL_DEFAULT_LLM_FLAVOR"] = ""
	env["PORTAL_DEFAULT_LLM_BASE_URL"] = "file:///etc/passwd"
	if _, err := Parse(func(k string) string { return env[k] }); err == nil {
		t.Fatal("non-http base url accepted")
	}
}
