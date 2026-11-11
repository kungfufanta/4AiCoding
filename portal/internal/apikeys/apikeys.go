// Package apikeys implements the agent API keys (PORTAL_PLAN 3.2):
// format sbx_live_<keyid>_<secret>, only key_id + sha256(secret) are
// stored (plan 1 #5 — the secret is shown once at creation), a 60 s
// cache so a revoked key is refused within the plan's ≤ 60 s budget,
// and scope helpers shared with the agent-token issuer.
//
// The DB lookup runs through the SECURITY DEFINER api_key_verify()
// function (migration 0004): the key itself is the identity, so there
// is no app.org_id to pin and RLS would filter every row away. The
// function returns a row only for a live key whose secret matches.
package apikeys

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Prefix marks a live portal API key; secret scanning registers it
// (PORTAL_PLAN 7).
const Prefix = "sbx_live_"

// KeyIDLen is the length of the public key id (base32, 12 chars).
const KeyIDLen = 12

// ErrMalformed — the bearer value is not sbx_live_<id>_<secret>.
var ErrMalformed = errors.New("malformed api key")

// ErrInvalid — well-formed but unknown, wrong secret, revoked or expired.
var ErrInvalid = errors.New("invalid api key")

var (
	keyIDRe  = regexp.MustCompile(`^[A-Z2-7]{12}$`)
	secretRe = regexp.MustCompile(`^[A-Za-z0-9]{40,}$`)
)

// Scopes the plan v1 API knows (PORTAL_PLAN 5). Keys request a subset.
var KnownScopes = []string{"sandbox:create", "sandbox:read", "sandbox:exec", "usage:read"}

// ScopeValid reports whether s is a known scope.
func ScopeValid(s string) bool {
	for _, k := range KnownScopes {
		if k == s {
			return true
		}
	}
	return false
}

// ScopesSubset reports whether every scope in want is granted in have.
func ScopesSubset(want, have []string) bool {
	set := make(map[string]bool, len(have))
	for _, h := range have {
		set[h] = true
	}
	for _, w := range want {
		if !set[w] {
			return false
		}
	}
	return true
}

// Key is a verified key's identity payload.
type Key struct {
	ID        string // uuid of the api_keys row
	KeyID     string // public sbx_live id
	OrgID     string
	ProjectID string // "" when unrestricted
	Scopes    []string
	ExpiresAt time.Time // zero when the key never expires
}

// Parse splits a presented key into its key id and secret part.
func Parse(presented string) (keyID, secret string, err error) {
	if len(presented) <= len(Prefix) || presented[:len(Prefix)] != Prefix {
		return "", "", ErrMalformed
	}
	rest := presented[len(Prefix):]
	for i := 0; i < len(rest); i++ {
		if rest[i] == '_' {
			keyID, secret = rest[:i], rest[i+1:]
			break
		}
	}
	if keyID == "" || secret == "" ||
		!keyIDRe.MatchString(keyID) || !secretRe.MatchString(secret) {
		return "", "", ErrMalformed
	}
	return keyID, secret, nil
}

// Format assembles a full key string from its parts.
func Format(keyID, secret string) string {
	return Prefix + keyID + "_" + secret
}

// HashSecret returns the hex sha256 of the secret part.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return fmt.Sprintf("%x", sum)
}

// VerifyHash compares a presented secret against a stored hash in
// constant time.
func VerifyHash(storedHashHex, secret string) bool {
	want := HashSecret(secret)
	return subtle.ConstantTimeCompare([]byte(storedHashHex), []byte(want)) == 1
}

const base62Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
const base32Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567"

// Generate mints a fresh key: returns the full presented string plus
// its parts. ~256 bits of secret entropy (43 × log2 62).
func Generate() (full, keyID, secret string, err error) {
	keyID, err = randomString(KeyIDLen, base32Alphabet)
	if err != nil {
		return "", "", "", fmt.Errorf("generate key id: %w", err)
	}
	secret, err = randomString(43, base62Alphabet)
	if err != nil {
		return "", "", "", fmt.Errorf("generate secret: %w", err)
	}
	return Format(keyID, secret), keyID, secret, nil
}

// randomString picks n characters uniformly (rejection sampling, so
// no modulo bias).
func randomString(n int, alphabet string) (string, error) {
	out := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range out {
		for {
			v, err := rand.Int(rand.Reader, max)
			if err != nil {
				return "", err
			}
			out[i] = alphabet[v.Int64()]
			break
		}
	}
	return string(out), nil
}

// Verifier answers Bearer verification with a 60 s in-memory cache.
type Verifier struct {
	Pool *pgxpool.Pool
	Log  *slog.Logger
	// TTL is the cache lifetime; a revoked or expired key keeps
	// authenticating at most this long (plan 3.2: ≤ 60 s). Zero = 60 s.
	TTL time.Duration

	mu    sync.RWMutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	key      Key
	ok       bool
	cachedAt time.Time
}

func (v *Verifier) ttl() time.Duration {
	if v.TTL > 0 {
		return v.TTL
	}
	return 60 * time.Second
}

// Verify resolves a presented full key string. Invalid keys get a
// negative cache entry, so a flood of bad keys does not hammer
// Postgres; entries live for TTL either way.
func (v *Verifier) Verify(ctx context.Context, presented string) (Key, error) {
	keyID, secret, err := Parse(presented)
	if err != nil {
		return Key{}, err
	}

	if v.cache != nil {
		v.mu.RLock()
		entry, hit := v.cache[keyID]
		v.mu.RUnlock()
		if hit && time.Since(entry.cachedAt) < v.ttl() {
			if !entry.ok {
				return Key{}, ErrInvalid
			}
			// Expiry checked at cache time can lapse inside the
			// TTL window — re-check before honouring the entry.
			if !entry.key.ExpiresAt.IsZero() && !time.Now().Before(entry.key.ExpiresAt) {
				v.Invalidate(keyID)
				return Key{}, ErrInvalid
			}
			return entry.key, nil
		}
	}

	key, err := v.lookup(ctx, keyID, secret)
	if err != nil {
		v.remember(keyID, cacheEntry{ok: false, cachedAt: time.Now()})
		return Key{}, err
	}
	v.remember(keyID, cacheEntry{key: key, ok: true, cachedAt: time.Now()})
	return key, nil
}

// Invalidate drops a cache entry (called on revoke, so revocation is
// effective immediately in the process that saw it).
func (v *Verifier) Invalidate(keyID string) {
	if v == nil || v.cache == nil {
		return
	}
	v.mu.Lock()
	delete(v.cache, keyID)
	v.mu.Unlock()
}

func (v *Verifier) remember(keyID string, entry cacheEntry) {
	if v == nil {
		return
	}
	if v.cache == nil {
		v.cache = make(map[string]cacheEntry)
	}
	v.mu.Lock()
	v.cache[keyID] = entry
	v.mu.Unlock()
}

// lookup calls the SECURITY DEFINER verifier and maps the row. The
// function returns only identity columns — key_id comes from the
// presented key itself (they match by construction).
func (v *Verifier) lookup(ctx context.Context, keyID, secret string) (Key, error) {
	var (
		row       Key
		expiresAt *time.Time
	)
	err := v.Pool.QueryRow(ctx,
		`SELECT id::text, org_id::text, COALESCE(project_id::text, ''), scopes, expires_at
             FROM api_key_verify($1, $2)`,
		keyID, secret).Scan(&row.ID, &row.OrgID, &row.ProjectID, &row.Scopes, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Key{}, ErrInvalid
	}
	if err != nil {
		if v.Log != nil {
			v.Log.Error("api key verify failed", "error", err)
		}
		return Key{}, fmt.Errorf("verify api key: %w", err)
	}
	row.KeyID = keyID
	if expiresAt != nil {
		row.ExpiresAt = *expiresAt
	}
	return row, nil
}
