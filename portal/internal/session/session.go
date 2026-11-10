// Package session implements the Portal's web sessions
// (PORTAL_PLAN 3.1): the BFF mints a 32-byte random token, stores
// only its sha256 in web_sessions, and hands the browser an
// HttpOnly, SameSite=Lax cookie. A second random value rides in a
// JS-readable cookie as the double-submit CSRF token, bound to the
// session via its stored hash.
package session

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Cookie names. SessionCookie is HttpOnly (the server reads it);
// CSRFCookie is readable by the SPA, which must echo it back in the
// X-CSRF-Token header (double submit, plan 3.1).
const (
	SessionCookie = "portal_session"
	CSRFCookie    = "portal_csrf"
)

// ErrNotFound is returned for unknown/expired/tampered tokens.
var ErrNotFound = errors.New("session not found")

// Manager stores sessions in Postgres.
type Manager struct {
	Pool *pgxpool.Pool
	TTL  time.Duration
}

// New returns a Manager with the given lifetime.
func New(pool *pgxpool.Pool, ttl time.Duration) *Manager {
	return &Manager{Pool: pool, TTL: ttl}
}

func randomToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("session: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// Session is the resolved server-side state for one browser session.
type Session struct {
	UserID   string
	CSRFHash string
	Expires  time.Time
}

// Create mints a new session for userID and returns the plaintext
// (token, csrf) pair exactly once — only their hashes are stored
// (PORTAL_PLAN 1 #5). Expired sessions are swept opportunistically.
func (m *Manager) Create(ctx context.Context, userID string) (token, csrf string, err error) {
	token, csrf = randomToken(), randomToken()
	expires := time.Now().Add(m.TTL)
	if _, err := m.Pool.Exec(ctx,
		`DELETE FROM web_sessions WHERE expires_at < now()`); err != nil {
		return "", "", fmt.Errorf("sweep expired web_sessions: %w", err)
	}
	if _, err := m.Pool.Exec(ctx, `
                INSERT INTO web_sessions (id_hash, user_id, csrf_hash, expires_at)
                VALUES ($1, $2, $3, $4)`,
		hash(token), userID, hash(csrf), expires); err != nil {
		return "", "", fmt.Errorf("insert web_session: %w", err)
	}
	return token, csrf, nil
}

// Get resolves a cookie token to its session. Wrong or expired
// tokens yield ErrNotFound (the handler answers 401 without
// distinguishing).
func (m *Manager) Get(ctx context.Context, token string) (Session, error) {
	var s Session
	err := m.Pool.QueryRow(ctx, `
                SELECT user_id::text, csrf_hash, expires_at
                FROM web_sessions
                WHERE id_hash = $1 AND expires_at > now()`,
		hash(token)).Scan(&s.UserID, &s.CSRFHash, &s.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	if err != nil {
		return Session{}, fmt.Errorf("load web_session: %w", err)
	}
	return s, nil
}

// CSRFHash returns the stored hash for a token (used by the auth
// middleware to validate the double-submit pair without a second
// query).
func CSRFHash(token string) string { return hash(token) }

// Delete removes a session (logout).
func (m *Manager) Delete(ctx context.Context, token string) error {
	_, err := m.Pool.Exec(ctx, `DELETE FROM web_sessions WHERE id_hash = $1`, hash(token))
	if err != nil {
		return fmt.Errorf("delete web_session: %w", err)
	}
	return nil
}
