package agenttoken

import (
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
)

func testKey() apikeys.Key {
	return apikeys.Key{
		ID:     "00000000-0000-0000-0000-000000000001",
		KeyID:  "AAAAAAAAAAAA",
		OrgID:  "00000000-0000-0000-0000-000000000002",
		Scopes: []string{"sandbox:read", "sandbox:exec"},
	}
}

func TestMintVerifyRoundtrip(t *testing.T) {
	s, err := NewSigner()
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	signed, exp, err := s.Mint(testKey(), []string{"sandbox:read"}, 0, "run-42")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if time.Until(exp) <= 0 || time.Until(exp) > DefaultTTL+time.Minute {
		t.Fatalf("expiry %v outside expected window", exp)
	}
	claims, err := s.Verify(signed)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if claims.KeyID != "AAAAAAAAAAAA" || claims.OrgID != testKey().OrgID {
		t.Errorf("claims identity mismatch: %+v", claims)
	}
	if len(claims.Scopes) != 1 || claims.Scopes[0] != "sandbox:read" {
		t.Errorf("claims scopes = %v", claims.Scopes)
	}
	if claims.AgentRunID != "run-42" {
		t.Errorf("agent_run_id = %q", claims.AgentRunID)
	}
}

func TestMintClampsTTL(t *testing.T) {
	s, err := NewSigner()
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// Request 10 minutes — above MaxTTL — the token must still expire
	// within MaxTTL + slack.
	_, exp, err := s.Mint(testKey(), nil, 10*time.Minute, "")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if time.Until(exp) > MaxTTL+time.Minute {
		t.Fatalf("TTL not clamped: %v", time.Until(exp))
	}
	// Request 1 second — floored to MinTTL.
	_, exp2, err := s.Mint(testKey(), nil, time.Second, "")
	if err != nil {
		t.Fatalf("Mint small: %v", err)
	}
	if time.Until(exp2) < MinTTL-10*time.Second {
		t.Fatalf("TTL not floored: %v", time.Until(exp2))
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	s, err := NewSigner()
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	signed, _, err := s.Mint(testKey(), nil, 0, "")
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	// Flip a payload character.
	parts := strings.Split(signed, ".")
	body := []byte(parts[1])
	if body[0] == 'e' {
		body[0] = 'f'
	} else {
		body[0] = 'e'
	}
	parts[1] = string(body)
	if _, err := s.Verify(strings.Join(parts, ".")); err == nil {
		t.Error("tampered token accepted")
	}
	// A different signer's key must not verify.
	other, err := NewSigner()
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	if _, err := other.Verify(signed); err == nil {
		t.Error("token verified against the wrong signer")
	}
}

func TestVerifyRejectsWrongAlgorithm(t *testing.T) {
	s, err := NewSigner()
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// Hand-roll an HS256 token carrying the same claims.
	claims := jwt.MapClaims{"sub": "AAAAAAAAAAAA", "org_id": "org", "exp": time.Now().Add(time.Hour).Unix()}
	hsToken, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("attacker-key"))
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	if _, err := s.Verify(hsToken); err == nil {
		t.Error("HS256 token accepted by an EdDSA-only verifier")
	}
	// alg:none style token (unsigned).
	noneToken, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	if _, err := s.Verify(noneToken); err == nil {
		t.Error("unsigned token accepted")
	}
}

func TestVerifyRejectsExpired(t *testing.T) {
	s, err := NewSigner()
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	// Mint with the minimum TTL and travel past it by hand-crafting
	// an already-expired token signed with the real key.
	key := testKey()
	now := time.Now().Add(-2 * time.Hour)
	claims := jwt.MapClaims{
		"sub": key.KeyID, "org_id": key.OrgID,
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(),
	}
	expired, err := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims).SignedString(s.priv)
	if err != nil {
		t.Fatalf("sign expired: %v", err)
	}
	if _, err := s.Verify(expired); err == nil {
		t.Error("expired token accepted")
	}
}
