// LLM connection CRUD + test + models (AGENT_PLAN A2 §5.2). Two
// surfaces share the same logic:
//
//	/api/admin/llm/connections   — platform admin only (org_id NULL).
//	/api/orgs/{id}/llm/connections — org owner/admin edit, member
//	                                read; org_id = the path org.
//
// Both surfaces expose the same JSON shape; the difference is the RLS
// scope and the role gate. The /v1 surface mirrors the org endpoints
// for agent callers (Bearer API key with scope "llm:read" / "llm:write").
//
// Key handling: the plaintext key NEVER appears in a response. The
// create/update body carries api_key (plaintext) once; it is
// AES-GCM encrypted before storage, and the response returns only
// has_api_key + key_label (last 4 chars). A subsequent update with a
// blank api_key leaves the stored key untouched.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/llmconfig"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// llmConnectionJSON is the wire shape. api_key is write-only (create
// + update); has_api_key + key_label are read-only.
type llmConnectionJSON struct {
	ID           string            `json:"id"`
	OrgID        string            `json:"org_id,omitempty"` // "" for platform
	Name         string            `json:"name"`
	Protocol     string            `json:"protocol"`
	Flavor       string            `json:"flavor"`
	BaseURL      string            `json:"base_url"`
	DefaultModel string            `json:"default_model"`
	Models       []llmconfig.Model `json:"models"`
	KeyMode      string            `json:"key_mode"`
	TimeoutS     int               `json:"timeout_s"`
	MaxTokens    int               `json:"max_tokens"`
	Enabled      bool              `json:"enabled"`
	IsDefault    bool              `json:"is_default"`
	HasKey       bool              `json:"has_api_key"`
	KeyLabel     string            `json:"key_label,omitempty"`
	APIKey       string            `json:"api_key,omitempty"` // write-only
}

// handleListLLMConnections — GET /api/admin/llm/connections (platform)
// or GET /api/orgs/{orgID}/llm/connections (org). The scope parameter
// selects which rows to return.
func (s *Server) handleListLLMConnections(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID") // "" for platform
	platformAdmin := orgID == ""
	if platformAdmin {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
	} else {
		if _, ok := s.requireMember(w, r, orgID); !ok {
			return
		}
	}
	rows, err := s.listLLMConnections(r.Context(), orgID)
	if err != nil {
		s.Log.Error("list llm connections failed", "error", err.Error())
		writeErr(w, http.StatusInternalServerError, "internal", "could not list connections")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": rows})
}

// handleCreateLLMConnection — POST /api/admin/llm/connections or
// /api/orgs/{orgID}/llm/connections.
func (s *Server) handleCreateLLMConnection(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	platformAdmin := orgID == ""
	actorID := db.UserID(r.Context())
	if platformAdmin {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
	} else {
		if role, ok := s.requireMember(w, r, orgID); !ok || role != "owner" && role != "admin" {
			if ok {
				writeErr(w, http.StatusForbidden, "forbidden", "only owner/admin can create connections")
			}
			return
		}
	}
	var body llmConnectionJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !validateLLMConnection(w, &body, platformAdmin) {
		return
	}
	// For org connections, refuse private base_urls (SSRF guard).
	// Platform connections (admin-created) may point internally.
	if !platformAdmin && llmconfig.IsPrivateBaseURL(body.BaseURL) {
		writeErr(w, http.StatusBadRequest, "ssrf_blocked",
			"base_url must not resolve to a private/loopback IP")
		return
	}
	conn, err := s.upsertLLMConnection(r.Context(), orgID, actorID, &body, "")
	if err != nil {
		s.writeLLMStoreErr(w, err)
		return
	}
	body.APIKey = "" // never echo back
	writeJSON(w, http.StatusCreated, llmConnectionJSON{
		ID: conn.ID, OrgID: conn.OrgID, Name: conn.Name, Protocol: conn.Protocol,
		Flavor: conn.Flavor, BaseURL: conn.BaseURL, DefaultModel: conn.DefaultModel,
		Models: conn.Models, KeyMode: conn.KeyMode, TimeoutS: conn.TimeoutS,
		MaxTokens: conn.MaxTokens, Enabled: conn.Enabled, IsDefault: conn.IsDefault,
		HasKey: body.HasKey, KeyLabel: body.KeyLabel,
	})
}

// handleUpdateLLMConnection — PATCH .../{cid}.
func (s *Server) handleUpdateLLMConnection(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	cid := r.PathValue("cid")
	platformAdmin := orgID == ""
	if platformAdmin {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
	} else {
		if role, ok := s.requireMember(w, r, orgID); !ok || role != "owner" && role != "admin" {
			if ok {
				writeErr(w, http.StatusForbidden, "forbidden", "only owner/admin can edit connections")
			}
			return
		}
	}
	var body llmConnectionJSON
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "invalid JSON body")
		return
	}
	if !validateLLMConnection(w, &body, platformAdmin) {
		return
	}
	if !platformAdmin && llmconfig.IsPrivateBaseURL(body.BaseURL) {
		writeErr(w, http.StatusBadRequest, "ssrf_blocked",
			"base_url must not resolve to a private/loopback IP")
		return
	}
	conn, err := s.upsertLLMConnection(r.Context(), orgID, db.UserID(r.Context()), &body, cid)
	if err != nil {
		s.writeLLMStoreErr(w, err)
		return
	}
	body.APIKey = ""
	writeJSON(w, http.StatusOK, llmConnectionJSON{
		ID: conn.ID, OrgID: conn.OrgID, Name: conn.Name, Protocol: conn.Protocol,
		Flavor: conn.Flavor, BaseURL: conn.BaseURL, DefaultModel: conn.DefaultModel,
		Models: conn.Models, KeyMode: conn.KeyMode, TimeoutS: conn.TimeoutS,
		MaxTokens: conn.MaxTokens, Enabled: conn.Enabled, IsDefault: conn.IsDefault,
		HasKey: body.HasKey, KeyLabel: body.KeyLabel,
	})
}

// handleDeleteLLMConnection — DELETE .../{cid}.
func (s *Server) handleDeleteLLMConnection(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	cid := r.PathValue("cid")
	platformAdmin := orgID == ""
	if platformAdmin {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
	} else {
		if role, ok := s.requireMember(w, r, orgID); !ok || role != "owner" && role != "admin" {
			if ok {
				writeErr(w, http.StatusForbidden, "forbidden", "only owner/admin can delete connections")
			}
			return
		}
	}
	err := s.withLLMTx(r.Context(), nonEmptyOrg(orgID), func(tx db.Tx) error {
		_, err := tx.Exec(r.Context(), `DELETE FROM model_connections WHERE id = $1::uuid`, cid)
		return err
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not delete connection")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleTestLLMConnection — POST .../{cid}/test. Loads the connection,
// decrypts its key, sends a tiny prompt, returns the result.
func (s *Server) handleTestLLMConnection(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	cid := r.PathValue("cid")
	platformAdmin := orgID == ""
	if platformAdmin {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
	} else {
		if _, ok := s.requireMember(w, r, orgID); !ok {
			return
		}
	}
	conn, key, err := s.loadConnectionForKeyedTest(r.Context(), orgID, cid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "connection not found")
		return
	}
	client := &llm.Client{
		BaseURL: conn.BaseURL, APIKey: key, Model: conn.DefaultModel,
		Flavor: conn.Flavor, HTTP: s.testHTTPClient(),
	}
	result := client.Test(r.Context(), conn.DefaultModel)
	writeJSON(w, http.StatusOK, result)
}

// handleListLLMModels — GET .../{cid}/models. Calls the provider's
// /v1/models and returns the id list (used by the "Load models"
// button in the connection editor).
func (s *Server) handleListLLMModels(w http.ResponseWriter, r *http.Request) {
	orgID := r.PathValue("orgID")
	cid := r.PathValue("cid")
	platformAdmin := orgID == ""
	if platformAdmin {
		if !s.requirePlatformAdmin(w, r) {
			return
		}
	} else {
		if _, ok := s.requireMember(w, r, orgID); !ok {
			return
		}
	}
	conn, key, err := s.loadConnectionForKeyedTest(r.Context(), orgID, cid)
	if err != nil {
		writeErr(w, http.StatusNotFound, "not_found", "connection not found")
		return
	}
	client := &llm.Client{
		BaseURL: conn.BaseURL, APIKey: key, Model: conn.DefaultModel,
		Flavor: conn.Flavor, HTTP: s.testHTTPClient(),
	}
	models, err := client.ListModels(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "upstream_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"models": models})
}

// --- helpers ---------------------------------------------------------

// requirePlatformAdmin gates the /api/admin/* routes. The current
// implementation uses the bootstrap-admin model: any signed-in user
// who is a member of the platform-admin org (a future flag) passes.
// Until that flag exists, only the bootstrap admin (password login)
// is a platform admin — detected by the session user being the
// bootstrap admin id stored in config.
func (s *Server) requirePlatformAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !s.PlatformAdminEnabled {
		writeErr(w, http.StatusForbidden, "forbidden", "platform admin endpoints are not enabled")
		return false
	}
	// The bootstrap admin is the only platform admin for now. A2
	// will add a platform_admins table later; until then, the
	// session user must be the bootstrap admin.
	userID := db.UserID(r.Context())
	if userID == "" || userID != s.BootstrapAdminID {
		writeErr(w, http.StatusForbidden, "forbidden", "platform admin only")
		return false
	}
	return true
}

// testHTTPClient returns the HTTP client used for LLM test/models
// calls. The default 10s timeout fits a quick probe.
func (s *Server) testHTTPClient() *http.Client {
	if s.LLMHTTP != nil {
		return s.LLMHTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// loadConnectionForKeyedTest loads one connection + its decrypted key.
func (s *Server) loadConnectionForKeyedTest(ctx context.Context, orgID, cid string) (llmconfig.Connection, string, error) {
	var conn llmconfig.Connection
	var key string
	err := s.withLLMTx(ctx, nonEmptyOrg(orgID), func(tx db.Tx) error {
		var modelsJSON []byte
		err := tx.QueryRow(ctx, `
                        SELECT id, COALESCE(org_id::text,''), name, protocol, flavor, base_url,
                               default_model, models, key_mode, timeout_s, max_tokens, enabled, is_default
                        FROM model_connections WHERE id = $1::uuid`, cid).Scan(
			&conn.ID, &conn.OrgID, &conn.Name, &conn.Protocol, &conn.Flavor, &conn.BaseURL,
			&conn.DefaultModel, &modelsJSON, &conn.KeyMode, &conn.TimeoutS, &conn.MaxTokens,
			&conn.Enabled, &conn.IsDefault)
		if err != nil {
			return err
		}
		if len(modelsJSON) > 0 {
			_ = json.Unmarshal(modelsJSON, &conn.Models)
		}
		// Load the key: shared → model_connections.api_key_enc;
		// per_org → model_connection_keys for this org.
		var enc []byte
		if conn.KeyMode == "shared" || orgID == "" {
			err = tx.QueryRow(ctx, `SELECT api_key_enc FROM model_connections WHERE id = $1::uuid`, cid).Scan(&enc)
		} else {
			err = tx.QueryRow(ctx, `
                                SELECT api_key_enc FROM model_connection_keys
                                WHERE connection_id = $1::uuid AND org_id = $2::uuid`,
				cid, orgID).Scan(&enc)
		}
		if err != nil {
			return err
		}
		plain, err := llmconfig.Decrypt(enc)
		if err != nil {
			return err
		}
		key = string(plain)
		return nil
	})
	return conn, key, err
}

// listLLMConnections loads all connections visible in the scope.
func (s *Server) listLLMConnections(ctx context.Context, orgID string) ([]llmConnectionJSON, error) {
	var out []llmConnectionJSON
	err := s.withLLMTx(ctx, nonEmptyOrg(orgID), func(tx db.Tx) error {
		rows, err := tx.Query(ctx, `
                        SELECT id, COALESCE(org_id::text,''), name, protocol, flavor, base_url,
                               default_model, models, key_mode, timeout_s, max_tokens, enabled, is_default
                        FROM model_connections
                        WHERE $1::text = '' OR org_id = $1::uuid
                        ORDER BY (org_id IS NULL) DESC, name`,
			nonEmptyOrg(orgID))
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c llmConnectionJSON
			var modelsJSON []byte
			if err := rows.Scan(&c.ID, &c.OrgID, &c.Name, &c.Protocol, &c.Flavor,
				&c.BaseURL, &c.DefaultModel, &modelsJSON, &c.KeyMode, &c.TimeoutS,
				&c.MaxTokens, &c.Enabled, &c.IsDefault); err != nil {
				return err
			}
			if len(modelsJSON) > 0 {
				_ = json.Unmarshal(modelsJSON, &c.Models)
			}
			c.HasKey = s.connectionHasKey(ctx, tx, c.ID, c.OrgID, c.KeyMode)
			c.KeyLabel = s.connectionKeyLabel(ctx, tx, c.ID, c.OrgID, c.KeyMode)
			out = append(out, c)
		}
		return rows.Err()
	})
	return out, err
}

// connectionHasKey reports whether a key exists for the connection.
// For key_mode = shared, the key is on model_connections.api_key_enc;
// for key_mode = per_org, it's in model_connection_keys for this org.
func (s *Server) connectionHasKey(ctx context.Context, tx db.Tx, connID, orgID, keyMode string) bool {
	if keyMode == "shared" || orgID == "" {
		var has bool
		err := tx.QueryRow(ctx, `SELECT api_key_enc IS NOT NULL FROM model_connections WHERE id = $1::uuid`, connID).Scan(&has)
		return err == nil && has
	}
	var exists bool
	err := tx.QueryRow(ctx, `
                SELECT EXISTS (
                        SELECT 1 FROM model_connection_keys
                        WHERE connection_id = $1::uuid AND org_id = $2::uuid
                )`, connID, orgID).Scan(&exists)
	return err == nil && exists
}

// connectionKeyLabel returns the last 4 chars of the key for UI hints.
func (s *Server) connectionKeyLabel(ctx context.Context, tx db.Tx, connID, orgID, keyMode string) string {
	if keyMode == "shared" || orgID == "" {
		var label string
		err := tx.QueryRow(ctx, `SELECT key_label FROM model_connections WHERE id = $1::uuid`, connID).Scan(&label)
		if err != nil {
			return ""
		}
		return label
	}
	var label string
	err := tx.QueryRow(ctx, `
                SELECT key_label FROM model_connection_keys
                WHERE connection_id = $1::uuid AND org_id = $2::uuid`,
		connID, orgID).Scan(&label)
	if err != nil {
		return ""
	}
	return label
}

// upsertLLMConnection inserts or updates a connection + its key.
// When cid is empty, it inserts; otherwise it updates the row with
// that id. The key is stored AES-GCM encrypted; an empty api_key on
// update leaves the stored key untouched.
func (s *Server) upsertLLMConnection(ctx context.Context, orgID, actorID string, body *llmConnectionJSON, cid string) (llmconfig.Connection, error) {
	var conn llmconfig.Connection
	modelsJSON, _ := json.Marshal(body.Models)
	err := s.withLLMTx(ctx, nonEmptyOrg(orgID), func(tx db.Tx) error {
		// Clear other defaults in the same scope before setting this one.
		if body.IsDefault {
			if _, err := tx.Exec(ctx, `
                                UPDATE model_connections SET is_default = false
                                WHERE ($1::text = '' AND org_id IS NULL)
                                   OR ($1::text <> '' AND org_id = $1::uuid)`, orgID); err != nil {
				return err
			}
		}
		// For platform (orgID == ""), org_id is NULL; for org, it's
		// the org UUID. The INSERT branches below handle each.
		if cid == "" {
			// Insert: org_id is NULL for platform, UUID for org.
			// created_by is NULL when actorID is empty (shouldn't
			// happen with session auth, but guard anyway).
			var err error
			if orgID == "" {
				if actorID == "" {
					err = tx.QueryRow(ctx, `
                                                INSERT INTO model_connections
                                                        (org_id, name, protocol, flavor, base_url, default_model, models,
                                                         key_mode, timeout_s, max_tokens, enabled, is_default, created_by)
                                                VALUES (NULL, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULL)
                                                RETURNING id, COALESCE(org_id::text,''), name, protocol, flavor, base_url,
                                                          default_model, models, key_mode, timeout_s, max_tokens, enabled, is_default`,
						body.Name, body.Protocol, body.Flavor, body.BaseURL, body.DefaultModel,
						modelsJSON, body.KeyMode, body.TimeoutS, body.MaxTokens, body.Enabled, body.IsDefault).Scan(
						&conn.ID, &conn.OrgID, &conn.Name, &conn.Protocol, &conn.Flavor, &conn.BaseURL,
						&conn.DefaultModel, &modelsJSON, &conn.KeyMode, &conn.TimeoutS, &conn.MaxTokens,
						&conn.Enabled, &conn.IsDefault)
				} else {
					err = tx.QueryRow(ctx, `
                                                INSERT INTO model_connections
                                                        (org_id, name, protocol, flavor, base_url, default_model, models,
                                                         key_mode, timeout_s, max_tokens, enabled, is_default, created_by)
                                                VALUES (NULL, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12::uuid)
                                                RETURNING id, COALESCE(org_id::text,''), name, protocol, flavor, base_url,
                                                          default_model, models, key_mode, timeout_s, max_tokens, enabled, is_default`,
						body.Name, body.Protocol, body.Flavor, body.BaseURL, body.DefaultModel,
						modelsJSON, body.KeyMode, body.TimeoutS, body.MaxTokens, body.Enabled, body.IsDefault,
						actorID).Scan(
						&conn.ID, &conn.OrgID, &conn.Name, &conn.Protocol, &conn.Flavor, &conn.BaseURL,
						&conn.DefaultModel, &modelsJSON, &conn.KeyMode, &conn.TimeoutS, &conn.MaxTokens,
						&conn.Enabled, &conn.IsDefault)
				}
			} else {
				if actorID == "" {
					err = tx.QueryRow(ctx, `
                                                INSERT INTO model_connections
                                                        (org_id, name, protocol, flavor, base_url, default_model, models,
                                                         key_mode, timeout_s, max_tokens, enabled, is_default, created_by)
                                                VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, NULL)
                                                RETURNING id, COALESCE(org_id::text,''), name, protocol, flavor, base_url,
                                                          default_model, models, key_mode, timeout_s, max_tokens, enabled, is_default`,
						orgID, body.Name, body.Protocol, body.Flavor, body.BaseURL, body.DefaultModel,
						modelsJSON, body.KeyMode, body.TimeoutS, body.MaxTokens, body.Enabled, body.IsDefault).Scan(
						&conn.ID, &conn.OrgID, &conn.Name, &conn.Protocol, &conn.Flavor, &conn.BaseURL,
						&conn.DefaultModel, &modelsJSON, &conn.KeyMode, &conn.TimeoutS, &conn.MaxTokens,
						&conn.Enabled, &conn.IsDefault)
				} else {
					err = tx.QueryRow(ctx, `
                                                INSERT INTO model_connections
                                                        (org_id, name, protocol, flavor, base_url, default_model, models,
                                                         key_mode, timeout_s, max_tokens, enabled, is_default, created_by)
                                                VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13::uuid)
                                                RETURNING id, COALESCE(org_id::text,''), name, protocol, flavor, base_url,
                                                          default_model, models, key_mode, timeout_s, max_tokens, enabled, is_default`,
						orgID, body.Name, body.Protocol, body.Flavor, body.BaseURL, body.DefaultModel,
						modelsJSON, body.KeyMode, body.TimeoutS, body.MaxTokens, body.Enabled, body.IsDefault,
						actorID).Scan(
						&conn.ID, &conn.OrgID, &conn.Name, &conn.Protocol, &conn.Flavor, &conn.BaseURL,
						&conn.DefaultModel, &modelsJSON, &conn.KeyMode, &conn.TimeoutS, &conn.MaxTokens,
						&conn.Enabled, &conn.IsDefault)
				}
			}
			if err != nil {
				return err
			}
		} else {
			err := tx.QueryRow(ctx, `
                                UPDATE model_connections SET
                                        name = $2, protocol = $3, flavor = $4, base_url = $5, default_model = $6,
                                        models = $7, key_mode = $8, timeout_s = $9, max_tokens = $10,
                                        enabled = $11, is_default = $12, updated_at = now()
                                WHERE id = $1::uuid
                                RETURNING id, COALESCE(org_id::text,''), name, protocol, flavor, base_url,
                                          default_model, models, key_mode, timeout_s, max_tokens, enabled, is_default`,
				cid, body.Name, body.Protocol, body.Flavor, body.BaseURL, body.DefaultModel,
				modelsJSON, body.KeyMode, body.TimeoutS, body.MaxTokens, body.Enabled, body.IsDefault).Scan(
				&conn.ID, &conn.OrgID, &conn.Name, &conn.Protocol, &conn.Flavor, &conn.BaseURL,
				&conn.DefaultModel, &modelsJSON, &conn.KeyMode, &conn.TimeoutS, &conn.MaxTokens,
				&conn.Enabled, &conn.IsDefault)
			if err != nil {
				return err
			}
		}
		_ = json.Unmarshal(modelsJSON, &conn.Models)
		// Store the key if provided. key_mode = shared → store in
		// model_connections.api_key_enc (one key for every org);
		// key_mode = per_org → store in model_connection_keys
		// (one key per org). Platform connections always use shared
		// (no org_id to key against); per_org platform connections
		// get their per-org keys entered by the platform admin on
		// the org's behalf (no self-service API yet).
		if body.APIKey != "" {
			enc, err := llmconfig.Encrypt([]byte(body.APIKey))
			if err != nil {
				return err
			}
			label := llmconfig.LabelFromKey(body.APIKey)
			body.HasKey = true
			body.KeyLabel = label
			if body.KeyMode == "shared" || orgID == "" {
				// Shared key on the connection row itself.
				if _, err := tx.Exec(ctx, `
                                        UPDATE model_connections
                                        SET api_key_enc = $2, key_label = $3, updated_at = now()
                                        WHERE id = $1::uuid`,
					conn.ID, enc, label); err != nil {
					return err
				}
			}
			if body.KeyMode == "per_org" && orgID != "" {
				if _, err := tx.Exec(ctx, `
                                        INSERT INTO model_connection_keys (connection_id, org_id, api_key_enc, key_label)
                                        VALUES ($1::uuid, $2::uuid, $3, $4)
                                        ON CONFLICT (connection_id, org_id) DO UPDATE
                                                SET api_key_enc = $3, key_label = $4, rotated_at = now()`,
					conn.ID, orgID, enc, label); err != nil {
					return err
				}
			}
		} else {
			body.HasKey = s.connectionHasKey(ctx, tx, conn.ID, conn.OrgID, body.KeyMode)
			body.KeyLabel = s.connectionKeyLabel(ctx, tx, conn.ID, conn.OrgID, body.KeyMode)
		}
		// Audit. Skip for platform (orgID == "") because the audit
		// table's RLS requires app.org_id set, and platform actions
		// run without an org scope. A separate platform audit trail
		// is out of scope for A2.
		if orgID != "" {
			meta, _ := json.Marshal(map[string]any{
				"connection_id": conn.ID, "name": conn.Name, "platform": false,
			})
			if err := tenants.Audit(ctx, tx, "user", actorID, "llm.connection.upsert", conn.ID, string(meta)); err != nil {
				return err
			}
		}
		return nil
	})
	return conn, err
}

// writeLLMStoreErr maps a storage error to the right HTTP status.
func (s *Server) writeLLMStoreErr(w http.ResponseWriter, err error) {
	s.Log.Error("llm connection store failed", "error", err.Error())
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "connection not found")
		return
	}
	writeErr(w, http.StatusInternalServerError, "internal", "could not store connection: "+err.Error())
}

// validateLLMConnection answers 400 itself and returns false when the
// body is not buildable. platformAdmin skips the SSRF check (admin
// may point at internal LiteLLM).
func validateLLMConnection(w http.ResponseWriter, b *llmConnectionJSON, platformAdmin bool) bool {
	switch {
	case b.Name == "":
		writeErr(w, http.StatusBadRequest, "bad_request", "name is required")
	case b.Protocol != "openai" && b.Protocol != "anthropic":
		writeErr(w, http.StatusBadRequest, "bad_request", `protocol must be "openai" or "anthropic"`)
	case b.Flavor != "generic" && b.Flavor != "webhub":
		writeErr(w, http.StatusBadRequest, "bad_request", `flavor must be "generic" or "webhub"`)
	case b.BaseURL == "" || !strings.HasPrefix(b.BaseURL, "http"):
		writeErr(w, http.StatusBadRequest, "bad_request", "base_url must be an http(s) URL")
	case b.DefaultModel == "":
		writeErr(w, http.StatusBadRequest, "bad_request", "default_model is required")
	case b.KeyMode != "shared" && b.KeyMode != "per_org":
		writeErr(w, http.StatusBadRequest, "bad_request", `key_mode must be "shared" or "per_org"`)
	case b.TimeoutS < 1 || b.TimeoutS > 600:
		writeErr(w, http.StatusBadRequest, "bad_request", "timeout_s must be 1..600")
	case b.MaxTokens < 1 || b.MaxTokens > 32768:
		writeErr(w, http.StatusBadRequest, "bad_request", "max_tokens must be 1..32768")
	default:
		return true
	}
	return false
}

// withLLMTx runs fn in a transaction scoped to orgID. When orgID is
// "" (platform), the transaction does NOT set app.org_id, so the RLS
// policy `org_id IS NULL OR org_id = app.org_id` exposes the platform
// rows (org_id IS NULL branch) to the platform admin. When orgID is
// non-empty, it runs as a normal org-scoped transaction (RLS hides
// other orgs' rows).
func (s *Server) withLLMTx(ctx context.Context, orgID string, fn func(db.Tx) error) error {
	if orgID == "" {
		return db.WithTx(ctx, s.Pool, fn)
	}
	return db.WithOrg(ctx, s.Pool, orgID, fn)
}

// nonEmptyOrg returns orgID unchanged. It exists only to make the
// SQL parameter typing explicit at call sites; the platform path is
// selected by withLLMTx (above) based on "".
func nonEmptyOrg(orgID string) string { return orgID }
