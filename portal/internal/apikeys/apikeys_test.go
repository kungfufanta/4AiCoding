package apikeys

import (
	"strings"
	"testing"
)

// Unit tests for the key format and scope helpers (no DB). The fuzz
// table covers plan S2's "fuzz parse key không panic": Parse must
// answer an error, never panic, for any hostile input.
func TestParseAndGenerateRoundtrip(t *testing.T) {
	full, keyID, secret, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !strings.HasPrefix(full, Prefix) {
		t.Fatalf("generated key missing prefix: %q", full)
	}
	gotID, gotSecret, err := Parse(full)
	if err != nil {
		t.Fatalf("Parse(generated): %v", err)
	}
	if gotID != keyID || gotSecret != secret {
		t.Fatalf("roundtrip mismatch: got %q/%q want %q/%q", gotID, gotSecret, keyID, secret)
	}
	if len(keyID) != KeyIDLen {
		t.Fatalf("key id length = %d, want %d", len(keyID), KeyIDLen)
	}
	if !secretRe.MatchString(secret) {
		t.Fatalf("secret %q does not match its own charset rule", secret)
	}
}

func TestParseHostileInputs(t *testing.T) {
	valid, _, secret, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	mustFail := []string{
		"",
		"sbx_live_",
		"sbx_live_short_abc",
		"sbx_live_lowercaseabcdef_" + secret,  // key id must be base32 upper
		"sbx_live_AAAAAAAAAAAA short",         // space instead of underscore
		"sbx_live_AAAAAAAAAAAA_",              // empty secret
		"sbx_live_" + strings.Repeat("A", 12), // no underscore, no secret
		"sbx_live_AAAAAAAAAAAA_" + secret + "_extra",
		"sbx_test_AAAAAAAAAAAA_" + secret,
		"SBX_LIVE_AAAAAAAAAAAA_" + secret, // prefix is case-sensitive
		valid + valid,
		valid[:len(valid)-1] + "!",
		"sbx_live_AAA2-7AAAAAA_" + secret, // dash not in base32 alphabet
		"bearer " + valid,
		valid + "\n",
		"\x00sbx_live_AAAAAAAAAAAA_" + secret,
	}
	for _, m := range mustFail {
		gotID, gotSecret, err := Parse(m)
		if err == nil {
			t.Errorf("Parse(%q) = %q/%q, want error", m, gotID, gotSecret)
		}
	}
	// These look hostile but are legitimately well-formed: lowercase
	// secrets and over-long secrets satisfy the charset rule.
	mustParse := []string{
		"sbx_live_AAAAAAAAAAAA_walfgqdbynqzzlfli46vljjeazyznwemmeiuoe9dynm",
		"sbx_live_AAAAAAAAAAAA_" + strings.Repeat("a", 3000),
		valid,
	}
	for _, m := range mustParse {
		if _, _, err := Parse(m); err != nil {
			t.Errorf("Parse(%.40q…) = %v, want nil", m, err)
		}
	}
}

func TestParseAcceptsOnlyWellFormed(t *testing.T) {
	// A valid key with a longer-than-40-char secret still parses.
	long := strings.Repeat("a", 64)
	if _, _, err := Parse(Format("AAAAAAAAAAAA", long)); err != nil {
		t.Errorf("Parse(long valid secret) = %v, want nil", err)
	}
}

func TestVerifyHash(t *testing.T) {
	_, _, secret, err := Generate()
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	hash := HashSecret(secret)
	if !VerifyHash(hash, secret) {
		t.Error("VerifyHash(hash, secret) = false, want true")
	}
	if VerifyHash(hash, secret+"x") {
		t.Error("VerifyHash(hash, wrong secret) = true, want false")
	}
}

func TestScopesSubset(t *testing.T) {
	granted := []string{"sandbox:read", "sandbox:exec"}
	if !ScopesSubset(nil, granted) {
		t.Error("empty request should be a subset")
	}
	if !ScopesSubset([]string{"sandbox:read"}, granted) {
		t.Error("subset rejected")
	}
	if ScopesSubset([]string{"sandbox:read", "sandbox:create"}, granted) {
		t.Error("non-subset accepted")
	}
	if ScopesSubset([]string{"admin:*"}, granted) {
		t.Error("unknown scope accepted")
	}
}

func TestScopeValid(t *testing.T) {
	for _, s := range KnownScopes {
		if !ScopeValid(s) {
			t.Errorf("ScopeValid(%q) = false, want true", s)
		}
	}
	if ScopeValid("sandbox:delete") {
		t.Error("unknown scope accepted")
	}
}
