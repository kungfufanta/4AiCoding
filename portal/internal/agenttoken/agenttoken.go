// Short-lived agent tokens (PORTAL_PLAN 3.2): an API key exchanges
// itself for an EdDSA-signed JWT (default 15 min, max 60 min) that
// sub-agents carry instead of the long-lived key. Tokens carry a kid
// so the signing key can rotate without invalidating verifiers.
package agenttoken

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
)

const (
	// DefaultTTL is the plan's 15 minutes.
	DefaultTTL = 15 * time.Minute
	// MaxTTL is the plan's 60 minutes cap.
	MaxTTL = 60 * time.Minute
	// MinTTL floors the requested TTL so a client cannot mint
	// something instantly expired by accident.
	MinTTL = 60 * time.Second
)

// ErrBadToken — signature invalid, unknown kid, wrong algorithm or expired.
var ErrBadToken = errors.New("invalid agent token")

// Signer holds the current Ed25519 key pair.
type Signer struct {
	kid  string
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// NewSigner generates a fresh key pair; kid is the first 16 hex
// characters of sha256(public key).
func NewSigner() (*Signer, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate signing key: %w", err)
	}
	sum := sha256.Sum256(pub)
	return &Signer{
		kid:  fmt.Sprintf("%x", sum[:8]),
		priv: priv,
		pub:  pub,
	}, nil
}

// KID identifies the current signing key in token headers.
func (s *Signer) KID() string { return s.kid }

// Mint issues a token for a verified key. scopes must already be a
// subset of the key's scopes (the handler enforces that); ttl is
// clamped into [MinTTL, MaxTTL].
func (s *Signer) Mint(key apikeys.Key, scopes []string, ttl time.Duration, agentRunID string) (signed string, expiresAt time.Time, err error) {
	now := time.Now()
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	if ttl < MinTTL {
		ttl = MinTTL
	}
	expiresAt = now.Add(ttl)

	claims := jwt.MapClaims{
		"iss":    "portal",
		"sub":    key.KeyID,
		"org_id": key.OrgID,
		"scopes": scopes,
		"iat":    now.Unix(),
		"exp":    expiresAt.Unix(),
		"jti":    newJTI(),
	}
	if key.ProjectID != "" {
		claims["project_id"] = key.ProjectID
	}
	if agentRunID != "" {
		claims["agent_run_id"] = agentRunID
	}

	tok := jwt.NewWithClaims(jwt.SigningMethodEdDSA, claims)
	tok.Header["kid"] = s.kid
	signed, err = tok.SignedString(s.priv)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign agent token: %w", err)
	}
	return signed, expiresAt, nil
}

// Claims is the verified payload of an agent token.
type Claims struct {
	KeyID      string
	OrgID      string
	ProjectID  string
	Scopes     []string
	AgentRunID string
	ExpiresAt  time.Time
}

// Verify parses and validates a token against this signer's key.
// Only EdDSA tokens with a known kid are accepted.
func (s *Signer) Verify(token string) (Claims, error) {
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodEd25519); !ok {
			return nil, fmt.Errorf("%w: unexpected signing method %v", ErrBadToken, t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		if kid != s.kid {
			return nil, fmt.Errorf("%w: unknown kid", ErrBadToken)
		}
		return s.pub, nil
	}, jwt.WithValidMethods([]string{"EdDSA"}), jwt.WithExpirationRequired())
	if err != nil {
		return Claims{}, ErrBadToken
	}
	mc, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return Claims{}, ErrBadToken
	}
	out := Claims{
		KeyID:      str(mc["sub"]),
		OrgID:      str(mc["org_id"]),
		ProjectID:  str(mc["project_id"]),
		AgentRunID: str(mc["agent_run_id"]),
	}
	if out.KeyID == "" || out.OrgID == "" {
		return Claims{}, ErrBadToken
	}
	if exp := mc["exp"]; exp != nil {
		if f, ok := exp.(float64); ok {
			out.ExpiresAt = time.Unix(int64(f), 0)
		}
	}
	if scopes, ok := mc["scopes"].([]any); ok {
		out.Scopes = make([]string, 0, len(scopes))
		for _, s := range scopes {
			if v := str(s); v != "" {
				out.Scopes = append(out.Scopes, v)
			}
		}
	}
	return out, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// newJTI mints the token id; crypto/rand via uuid, with a hash
// fallback so minting never fails.
func newJTI() string {
	id, err := uuid.NewRandom()
	if err != nil {
		sum := sha256.Sum256([]byte(fmt.Sprint(time.Now().UnixNano())))
		return fmt.Sprintf("%x", sum[:16])
	}
	return id.String()
}
