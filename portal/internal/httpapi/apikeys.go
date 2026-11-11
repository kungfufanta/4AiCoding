// API key management on the dashboard BFF (PORTAL_PLAN 6/S2) plus the
// public POST /v1/tokens exchange. Secrets leave the server exactly
// once — in the create-key response body (plan 1 #5).
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// apiKeyJSON is the stored-key view; the secret hash never appears.
type apiKeyJSON struct {
	ID         string     `json:"id"`
	KeyID      string     `json:"key_id"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	ProjectID  *string    `json:"project_id"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	RevokedAt  *time.Time `json:"revoked_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (s *Server) handleListApiKeys(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	var keys []apiKeyJSON
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		rows, err := tx.Query(r.Context(), `
                        SELECT id::text, key_id, name, scopes, project_id::text,
                               expires_at, last_used_at, revoked_at, created_at
                        FROM api_keys
                        ORDER BY created_at DESC`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k apiKeyJSON
			var projectID *string
			if err := rows.Scan(&k.ID, &k.KeyID, &k.Name, &k.Scopes, &projectID,
				&k.ExpiresAt, &k.LastUsedAt, &k.RevokedAt, &k.CreatedAt); err != nil {
				return err
			}
			k.ProjectID = projectID
			keys = append(keys, k)
		}
		return rows.Err()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not list api keys")
		return
	}
	if keys == nil {
		keys = []apiKeyJSON{}
	}
	writeJSON(w, http.StatusOK, keys)
}

// handleCreateApiKey issues a key and returns the full secret once.
func (s *Server) handleCreateApiKey(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireAdmin(w, r, orgID); !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	var body struct {
		Name          string   `json:"name"`
		Scopes        []string `json:"scopes"`
		ProjectID     string   `json:"project_id"`
		ExpiresInDays int      `json:"expires_in_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request",
			`body must be {"name", "scopes", "project_id"?, "expires_in_days"?}`)
		return
	}
	body.Name = strings.TrimSpace(body.Name)
	if body.Name == "" || len(body.Name) > 100 {
		writeErr(w, http.StatusBadRequest, "bad_request", "name must be 1..100 characters")
		return
	}
	if len(body.Scopes) == 0 || len(body.Scopes) > len(apikeys.KnownScopes) {
		writeErr(w, http.StatusBadRequest, "bad_request", "at least one known scope is required")
		return
	}
	seen := make(map[string]bool, len(body.Scopes))
	for _, sc := range body.Scopes {
		if !apikeys.ScopeValid(sc) || seen[sc] {
			writeErr(w, http.StatusBadRequest, "bad_request", "unknown or duplicate scope: "+sc)
			return
		}
		seen[sc] = true
	}
	if body.ExpiresInDays < 0 || body.ExpiresInDays > 365 {
		writeErr(w, http.StatusBadRequest, "bad_request", "expires_in_days must be 0..365")
		return
	}

	// The project, when given, must be one of this org's (RLS makes a
	// foreign project invisible, so the probe below answers 400).
	var projectID *string
	if body.ProjectID != "" {
		var exists bool
		err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
			return tx.QueryRow(r.Context(),
				`SELECT EXISTS (SELECT 1 FROM projects WHERE id = $1::uuid)`,
				body.ProjectID).Scan(&exists)
		})
		if err != nil || !exists {
			writeErr(w, http.StatusBadRequest, "bad_request", "unknown project for this org")
			return
		}
		projectID = &body.ProjectID
	}

	full, keyID, secret, err := apikeys.Generate()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not generate key")
		return
	}
	var expiresAt *time.Time
	if body.ExpiresInDays > 0 {
		t := time.Now().AddDate(0, 0, body.ExpiresInDays)
		expiresAt = &t
	}

	var created apiKeyJSON
	err = db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		var id string
		var createdAt time.Time
		err := tx.QueryRow(r.Context(), `
                        INSERT INTO api_keys (org_id, project_id, name, key_id, secret_hash, scopes, expires_at, created_by)
                        VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8::uuid)
                        RETURNING id::text, created_at`,
			orgID, projectID, body.Name, keyID, apikeys.HashSecret(secret), body.Scopes, expiresAt,
			db.UserID(r.Context())).Scan(&id, &createdAt)
		if err != nil {
			return err
		}
		created = apiKeyJSON{
			ID: id, KeyID: keyID, Name: body.Name, Scopes: body.Scopes,
			ProjectID: projectID, ExpiresAt: expiresAt, CreatedAt: createdAt,
		}
		meta, _ := json.Marshal(map[string]any{"key_id": keyID, "name": body.Name, "scopes": body.Scopes})
		return tenants.Audit(r.Context(), tx, "user", db.UserID(r.Context()), "apikey.create", id, string(meta))
	})
	if err != nil {
		s.Log.Error("create api key failed", slog.Any("error", err))
		writeErr(w, http.StatusInternalServerError, "internal", "could not create api key")
		return
	}

	// Only place where the secret crosses the wire.
	writeJSON(w, http.StatusCreated, map[string]any{"key": created, "secret": full})
}

// handleRevokeApiKey retires a key immediately (and evicts it from
// the local verify cache).
func (s *Server) handleRevokeApiKey(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireAdmin(w, r, orgID); !ok {
		return
	}
	keyID := r.PathValue("keyID")
	var revokedID string
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(r.Context(), `
                        UPDATE api_keys SET revoked_at = now()
                        WHERE key_id = $1 AND revoked_at IS NULL
                        RETURNING id::text`, keyID).Scan(&revokedID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "api key not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not revoke api key")
		return
	}
	if err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		meta, _ := json.Marshal(map[string]string{"key_id": keyID})
		return tenants.Audit(r.Context(), tx, "user", db.UserID(r.Context()), "apikey.revoke", revokedID, string(meta))
	}); err != nil {
		s.Log.Warn("audit apikey.revoke failed", slog.Any("error", err))
	}
	if s.Keys != nil {
		s.Keys.Invalidate(keyID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"key_id": keyID, "status": "revoked"})
}

// bearerToken extracts the Authorization: Bearer value, or "".
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// handleAgentToken answers POST /v1/tokens: an API key exchanges
// itself for a short-lived agent token (PORTAL_PLAN 3.2). Requested
// scopes must be a subset of the key's; TTL clamps into
// [agenttoken.MinTTL, agenttoken.MaxTTL].
func (s *Server) handleAgentToken(w http.ResponseWriter, r *http.Request) {
	if s.Keys == nil || s.Tokens == nil {
		writeErr(w, http.StatusServiceUnavailable, "tokens_unavailable", "agent tokens are not configured")
		return
	}
	presented := bearerToken(r)
	if presented == "" {
		writeErr(w, http.StatusUnauthorized, "unauthorized", "API key required (Authorization: Bearer sbx_live_…)")
		return
	}
	key, err := s.Keys.Verify(r.Context(), presented)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "invalid or revoked API key")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 16384)
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "unreadable body")
		return
	}
	var body struct {
		Scopes     []string `json:"scopes"`
		TTLSeconds int      `json:"ttl_seconds"`
		AgentRunID string   `json:"agent_run_id"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &body); err != nil {
			writeErr(w, http.StatusBadRequest, "bad_request", "body must be JSON")
			return
		}
	}

	scopes := key.Scopes
	if len(body.Scopes) > 0 {
		if !apikeys.ScopesSubset(body.Scopes, key.Scopes) {
			writeErr(w, http.StatusForbidden, "scope_exceeded",
				"requested scopes exceed what the API key grants")
			return
		}
		scopes = body.Scopes
	}

	ttl := time.Duration(body.TTLSeconds) * time.Second
	token, expiresAt, err := s.Tokens.Mint(key, scopes, ttl, body.AgentRunID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not mint token")
		return
	}

	// Best-effort audit entry; token issuance must not fail because of it.
	if err := db.WithOrg(r.Context(), s.Pool, key.OrgID, func(tx db.Tx) error {
		meta, _ := json.Marshal(map[string]any{"key_id": key.KeyID, "scopes": scopes, "agent_run_id": body.AgentRunID})
		return tenants.Audit(r.Context(), tx, "api_key", key.KeyID, "agenttoken.issue", key.KeyID, string(meta))
	}); err != nil {
		s.Log.Warn("audit agenttoken.issue failed", slog.Any("error", err))
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token":      token,
		"token_type": "Bearer",
		"expires_at": expiresAt.UTC().Format(time.RFC3339),
		"scopes":     scopes,
	})
}
