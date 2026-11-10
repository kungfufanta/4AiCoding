package store

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// APIKey is the METADATA view of an API key (migrations/0022). It never carries
// the key_hash — the hash is written by CreateAPIKey and only read back for
// lookup, never selected into this struct.
type APIKey struct {
	ID         string
	Name       string
	Prefix     string
	CreatedAt  time.Time
	LastUsedAt *time.Time
	// Scopes restricts what the key may call; empty = full access.
	Scopes []string
	// Role caps what the key may do (admin | operator | viewer).
	Role string
	// ExpiresAt is when the key stops working; nil = never.
	ExpiresAt *time.Time
	// SandboxID binds the key to one sandbox; "" = any.
	SandboxID string
}

// ── sessions ─────────────────────────────────────────────────────────

// CreateSession inserts a session for a user, keyed by the sha256 hex of the
// cookie value.
func (s *Store) CreateSession(ctx context.Context, tokenHash, owner, userID string, createdAt, lastUsed, expires int64) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO console_session (token_hash, owner_token, user_id, created_at, last_used_at, expires_at)
			VALUES (?,?,?,?,?,?)`, tokenHash, owner, userID, createdAt, lastUsed, expires)
		return err
	})
}

// SessionInfo is what a session cookie resolves to.
type SessionInfo struct {
	Owner   string
	Expires int64
	User    *User
}

// LookupSession returns the session and its (enabled) user. found=false when
// there is no such session, it has no user, or the user is disabled. Expiry
// is enforced by the caller.
func (s *Store) LookupSession(ctx context.Context, tokenHash string) (SessionInfo, bool, error) {
	var si SessionInfo
	var userID sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT owner_token, expires_at, user_id FROM console_session WHERE token_hash = ?`, tokenHash).
		Scan(&si.Owner, &si.Expires, &userID)
	if err == sql.ErrNoRows {
		return si, false, nil
	}
	if err != nil {
		return si, false, err
	}
	if !userID.Valid {
		return si, false, nil
	}
	u, err := s.GetUser(ctx, userID.String)
	if err == ErrNotFound || (err == nil && u.Disabled) {
		return si, false, nil
	}
	if err != nil {
		return si, false, err
	}
	si.User = u
	return si, true, nil
}

// DeleteUserSessions signs a user out everywhere.
func (s *Store) DeleteUserSessions(ctx context.Context, userID string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `DELETE FROM console_session WHERE user_id = ?`, userID)
		return err
	})
}

// TouchSession bumps last_used_at (best-effort; ignore the returned error at the
// call site).
func (s *Store) TouchSession(ctx context.Context, tokenHash string, lastUsed int64) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx,
			`UPDATE console_session SET last_used_at = ? WHERE token_hash = ?`, lastUsed, tokenHash)
		return err
	})
}

// DeleteSession removes one session (logout).
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `DELETE FROM console_session WHERE token_hash = ?`, tokenHash)
		return err
	})
}

// DeleteAllSessions removes every session (sign out everywhere / password change).
func (s *Store) DeleteAllSessions(ctx context.Context) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `DELETE FROM console_session`)
		return err
	})
}

// ── API keys ─────────────────────────────────────────────────────────

const apiKeyMetaCols = `id, name, prefix, created_at, last_used_at, scopes, role, expires_at, sandbox_id`

// Scopes are stored comma-separated; ” round-trips to nil (full access).
func joinScopes(scopes []string) string { return strings.Join(scopes, ",") }

func splitScopes(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func scanAPIKey(sc scanner) (*APIKey, error) {
	k := &APIKey{}
	var created int64
	var lastUsed sql.NullInt64
	var scopes string
	var expires sql.NullInt64
	var sandboxID sql.NullString
	if err := sc.Scan(&k.ID, &k.Name, &k.Prefix, &created, &lastUsed, &scopes, &k.Role, &expires, &sandboxID); err != nil {
		return nil, err
	}
	if expires.Valid {
		t := time.Unix(expires.Int64, 0).UTC()
		k.ExpiresAt = &t
	}
	k.SandboxID = sandboxID.String
	k.CreatedAt = time.Unix(created, 0).UTC()
	if lastUsed.Valid {
		t := time.Unix(lastUsed.Int64, 0).UTC()
		k.LastUsedAt = &t
	}
	k.Scopes = splitScopes(scopes)
	return k, nil
}

// NewAPIKey is what CreateAPIKey stores. KeyHash is the sha256 hex of the
// plaintext (the store never sees plaintext). Empty Scopes = full access
// (within Role). ExpiresAt 0 = never; SandboxID "" = any sandbox.
type NewAPIKey struct {
	ID, Name, KeyHash, Prefix string
	Scopes                    []string
	Role                      string
	ExpiresAt                 int64
	SandboxID                 string
	CreatedAt                 int64
}

// CreateAPIKey inserts a key. ErrConflict when the name (or hash) exists.
func (s *Store) CreateAPIKey(ctx context.Context, k NewAPIKey) error {
	if k.Role == "" {
		k.Role = "admin"
	}
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `
			INSERT INTO api_key (id, name, key_hash, prefix, scopes, created_at, last_used_at, role, expires_at, sandbox_id)
			VALUES (?,?,?,?,?,?,NULL,?,?,?)`, k.ID, k.Name, k.KeyHash, k.Prefix, joinScopes(k.Scopes), k.CreatedAt,
			k.Role, sql.NullInt64{Int64: k.ExpiresAt, Valid: k.ExpiresAt > 0},
			sql.NullString{String: k.SandboxID, Valid: k.SandboxID != ""})
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return err
	})
}

// ListAPIKeys returns all keys as METADATA ONLY — the key_hash is never selected.
func (s *Store) ListAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+apiKeyMetaCols+` FROM api_key ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// LookupAPIKey returns the key metadata for a presented key's sha256 hash.
// found=false when no key matches. Expiry is enforced by the caller.
func (s *Store) LookupAPIKey(ctx context.Context, keyHash string) (*APIKey, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+apiKeyMetaCols+` FROM api_key WHERE key_hash = ?`, keyHash)
	k, err := scanAPIKey(row)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return k, true, nil
}

// TouchAPIKey bumps last_used_at (best-effort).
func (s *Store) TouchAPIKey(ctx context.Context, id string, lastUsed int64) error {
	return s.submit(ctx, func(db *sql.DB) error {
		_, err := db.ExecContext(ctx, `UPDATE api_key SET last_used_at = ? WHERE id = ?`, lastUsed, id)
		return err
	})
}

// DeleteAPIKey removes a key by id. Returns false when no such key exists (404).
func (s *Store) DeleteAPIKey(ctx context.Context, id string) (bool, error) {
	var deleted bool
	err := s.submit(ctx, func(db *sql.DB) error {
		res, err := db.ExecContext(ctx, `DELETE FROM api_key WHERE id = ?`, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted = n > 0
		return nil
	})
	return deleted, err
}
