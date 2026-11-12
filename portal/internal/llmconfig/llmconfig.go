// Package llmconfig resolves the effective LLM connection for an org
// (AGENT_PLAN A2 §5.2). The resolution order is:
//
//  1. the org's own is_default connection (most specific);
//  2. the platform's is_default connection (shared, every org sees it);
//  3. env PORTAL_DEFAULT_LLM_* (kept as a seed/fallback for dev stacks
//     that have not yet configured a platform connection in the DB).
//
// A connection carries the LLM provider config (protocol, flavor,
// base_url, default_model, models, timeout, max_tokens) and a key.
// The key is AES-256-GCM encrypted at rest with the master
// PORTAL_SECRETS_KEY; the API never returns the plaintext.
//
// For key_mode = per_org (WebHub), each org has its own key in
// model_connection_keys; for key_mode = shared, the key is the same
// for every org (the platform admin entered it once). The resolver
// returns a Keyed connection whose APIKey is the decrypted plaintext
// — the caller (internal/llm) sends it as the Bearer header and never
// logs it.
package llmconfig

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/argon2"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
)

// Connection is the in-memory shape of one model_connections row.
type Connection struct {
	ID           string
	OrgID        string // "" for platform connections
	Name         string
	Protocol     string // "openai" | "anthropic"
	Flavor       string // "generic" | "webhub"
	BaseURL      string
	DefaultModel string
	Models       []Model
	KeyMode      string // "shared" | "per_org"
	TimeoutS     int
	MaxTokens    int
	Enabled      bool
	IsDefault    bool
}

// Model is one entry in the connection's allowed model list.
type Model struct {
	ID            string `json:"id"`
	ContextWindow int    `json:"context_window,omitempty"`
	MaxOutput     int    `json:"max_output,omitempty"`
	SupportsTools bool   `json:"supports_tools,omitempty"`
}

// Keyed is a Connection plus the decrypted API key the caller sends
// to the provider. The key is the plaintext the caller sees; the DB
// stores only the AES-GCM ciphertext. Keyed values must never be
// logged.
type Keyed struct {
	Connection
	APIKey string
	// KeyLabel is the last 4 chars of the key (for UI hints only).
	KeyLabel string
}

// ErrNotFound is returned by ResolveEffective when no connection is
// configured for the org and no platform default exists.
var ErrNotFound = errors.New("no model connection configured")

// ResolveEffective returns the connection the org should use, in the
// resolution order above. The key is decrypted inside the same
// transaction so a rotation mid-request cannot return a stale key.
// Returns ErrNotFound when nothing is configured.
func ResolveEffective(ctx context.Context, pool *pgxpool.Pool, orgID string) (Keyed, error) {
	var k Keyed
	err := db.WithOrg(ctx, pool, orgID, func(tx pgx.Tx) error {
		// 1. Org's default connection.
		c, ok, err := loadDefaultConnection(ctx, tx, true)
		if err != nil {
			return err
		}
		if !ok {
			// 2. Platform's default connection.
			c, ok, err = loadDefaultConnection(ctx, tx, false)
			if err != nil {
				return err
			}
		}
		if !ok {
			return ErrNotFound
		}
		k.Connection = c
		// 3. Resolve the key.
		key, label, err := resolveKey(ctx, tx, c, orgID)
		if err != nil {
			return err
		}
		k.APIKey = key
		k.KeyLabel = label
		return nil
	})
	if err != nil {
		return Keyed{}, err
	}
	return k, nil
}

// loadDefaultConnection loads the is_default row. When orgScoped is
// true it looks for an org_id = current org row; otherwise for the
// platform row (org_id IS NULL). Returns ok=false when no default
// exists in that scope.
func loadDefaultConnection(ctx context.Context, tx pgx.Tx, orgScoped bool) (Connection, bool, error) {
	q := `
                SELECT id, COALESCE(org_id::text,''), name, protocol, flavor, base_url,
                       default_model, models, key_mode, timeout_s, max_tokens, enabled, is_default
                FROM model_connections
                WHERE is_default AND enabled`
	if orgScoped {
		q += ` AND org_id = NULLIF(current_setting('app.org_id', true), '')::uuid`
	} else {
		q += ` AND org_id IS NULL`
	}
	q += ` LIMIT 1`
	var c Connection
	var modelsJSON []byte
	err := tx.QueryRow(ctx, q).Scan(
		&c.ID, &c.OrgID, &c.Name, &c.Protocol, &c.Flavor, &c.BaseURL,
		&c.DefaultModel, &modelsJSON, &c.KeyMode, &c.TimeoutS, &c.MaxTokens,
		&c.Enabled, &c.IsDefault)
	if errors.Is(err, pgx.ErrNoRows) {
		return Connection{}, false, nil
	}
	if err != nil {
		return Connection{}, false, fmt.Errorf("load default connection: %w", err)
	}
	if len(modelsJSON) > 0 {
		_ = json.Unmarshal(modelsJSON, &c.Models)
	}
	return c, true, nil
}

// resolveKey returns the plaintext API key for the connection. For
// key_mode = shared, the key is on model_connections.api_key_enc
// (one key for every org). For key_mode = per_org, the key is in
// model_connection_keys for the calling org. Platform connections
// (org_id IS NULL) are always shared (no org_id to key against).
//
// The plaintext is AES-256-GCM decrypted with the master key. The
// master key derivation is in MasterKey(); the env var
// PORTAL_SECRETS_KEY is the raw 32-byte key (base64 or hex).
func resolveKey(ctx context.Context, tx pgx.Tx, c Connection, orgID string) (string, string, error) {
	var enc []byte
	var label string
	var err error
	if c.KeyMode == "shared" || c.OrgID == "" {
		// Shared key on the connection row itself.
		err = tx.QueryRow(ctx,
			`SELECT api_key_enc, key_label FROM model_connections WHERE id = $1`, c.ID).
			Scan(&enc, &label)
	} else {
		// Per-org key.
		err = tx.QueryRow(ctx, `
                        SELECT api_key_enc, key_label
                        FROM model_connection_keys
                        WHERE connection_id = $1 AND org_id = $2::uuid`,
			c.ID, orgID).Scan(&enc, &label)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", fmt.Errorf("no key configured for connection %s in org %s", c.ID, orgID)
	}
	if err != nil {
		return "", "", fmt.Errorf("load key: %w", err)
	}
	if enc == nil {
		return "", "", fmt.Errorf("no key configured for connection %s", c.ID)
	}
	plaintext, err := Decrypt(enc)
	if err != nil {
		return "", "", fmt.Errorf("decrypt key: %w", err)
	}
	return string(plaintext), label, nil
}

// --- AES-256-GCM -----------------------------------------------------

// masterKey caches the derived 32-byte AES key. The derivation is
// Argon2id from PORTAL_SECRETS_KEY so a short passphrase is still
// safe; a 32-byte raw key skips the KDF.
var (
	masterKey     []byte
	masterKeyInit bool
)

// MasterKey returns the AES-256 key derived from PORTAL_SECRETS_KEY.
// The env var may be:
//   - 32 raw bytes (base64 or hex);
//   - a passphrase (Argon2id, salt = "portal-llm-keys", 64 MiB, 3 iter).
//
// The result is cached for the process lifetime; rotating the env
// requires a restart.
func MasterKey() ([]byte, error) {
	if masterKeyInit {
		return masterKey, nil
	}
	env := getenv("PORTAL_SECRETS_KEY")
	if env == "" {
		return nil, errors.New("PORTAL_SECRETS_KEY not set; cannot decrypt LLM keys")
	}
	// Try base64 first (most ops will pass a raw 32-byte key).
	if b, err := base64.StdEncoding.DecodeString(env); err == nil && len(b) == 32 {
		masterKey = b
		masterKeyInit = true
		return masterKey, nil
	}
	// Try hex.
	if b := decodeHex(env); len(b) == 32 {
		masterKey = b
		masterKeyInit = true
		return masterKey, nil
	}
	// Fall back to Argon2id KDF (passphrase mode).
	masterKey = argon2.IDKey([]byte(env), []byte("portal-llm-keys"), 3, 64*1024, 4, 32)
	masterKeyInit = true
	return masterKey, nil
}

// Encrypt returns the AES-256-GCM ciphertext of plaintext.
func Encrypt(plaintext []byte) ([]byte, error) {
	key, err := MasterKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// Decrypt reverses Encrypt.
func Decrypt(ciphertext []byte) ([]byte, error) {
	key, err := MasterKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	if len(ciphertext) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ct := ciphertext[:gcm.NonceSize()], ciphertext[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ct, nil)
}

// --- SSRF guard ------------------------------------------------------

// IsPrivateBaseURL reports whether baseURL resolves to a private or
// loopback IP. Used to refuse org-supplied base_urls that point at
// the cluster's internal network (10.x, 169.254.169.254, ::1, …).
// Platform connections (created by the admin) are exempt.
//
// The check resolves DNS and walks every returned IP. A URL whose
// hostname resolves to a public IP but redirects to a private IP is
// caught at the HTTP client level (the llm.Client uses a custom
// DialContext that re-runs this check on every dial).
func IsPrivateBaseURL(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return true // unparseable → treat as private (refuse)
	}
	host := u.Hostname()
	if host == "" {
		return true
	}
	// Literal IP?
	if ip := net.ParseIP(host); ip != nil {
		return isPrivateIP(ip)
	}
	// Resolve and check every address.
	ips, err := net.LookupIP(host)
	if err != nil {
		return true // DNS failure → refuse (don't leak via error)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return true
		}
	}
	return false
}

// isPrivateIP reports whether ip is in a private/reserved range.
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	if ip.IsPrivate() { // 10/8, 172.16/12, 192.168/16, fc00::/7
		return true
	}
	// 169.254.169.254 (cloud metadata) is covered by IsLinkLocalUnicast.
	return false
}

// LabelFromKey returns the last 4 chars of the key for UI hints.
// Returns "" when the key is shorter than 4 chars.
func LabelFromKey(key string) string {
	if len(key) < 4 {
		return ""
	}
	return key[len(key)-4:]
}

// getenv wraps os.Getenv so tests can override it via getenvOverride.
var getenvOverride = os.Getenv

func getenv(key string) string { return getenvOverride(key) }

// decodeHex is a tiny hex decoder (avoids pulling encoding/hex into
// the exported API).
func decodeHex(s string) []byte {
	if len(s) != 64 { // 32 bytes = 64 hex chars
		return nil
	}
	out := make([]byte, 32)
	for i := 0; i < 32; i++ {
		hi, ok1 := hexNibble(s[i*2])
		lo, ok2 := hexNibble(s[i*2+1])
		if !ok1 || !ok2 {
			return nil
		}
		out[i] = hi<<4 | lo
	}
	return out
}

func hexNibble(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// Suppress "unused" when strings is only used in helpers.go.
var _ = strings.TrimSpace
