// Sandbox gateway core (PORTAL_PLAN 6/S3): the pieces shared by the
// public /v1 API and the dashboard BFF — the sandboxd reachability
// guard, ownership checks against the `sandboxes` index (404, never
// 403), idempotency replay and the read-through row sync that keeps
// the local index honest while sandboxd stays the source of truth.
package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/sandboxd"
	"github.com/tastyeffectco/sandboxd/portal/internal/tenants"
)

// maxExecSeconds bounds a dashboard/v1 exec server-side (the in-container
// timeout comes with AGENT_PLAN A0; until it merges sandboxd's legacy exec
// is cut off here, by request context).
const maxExecSeconds = 120

// sandboxJSON is the wire shape the two front doors return: the live
// sandboxd view plus the Portal-side project pointer.
type sandboxJSON struct {
	sandboxd.Sandbox
	ProjectID string `json:"project_id,omitempty"`
}

// writeSandboxdErr maps a sandboxd client error onto the portal's
// error envelope. 4xx are caller-fixable and relayed verbatim (minus
// sandboxd internals); 5xx and transport failures become 502 so the
// caller never sees sandboxd's internal detail.
func (s *Server) writeSandboxdErr(w http.ResponseWriter, err error) {
	var apiErr *sandboxd.APIError
	if errors.As(err, &apiErr) {
		if apiErr.Status >= 500 {
			s.Log.Error("sandboxd 5xx", "status", apiErr.Status, "code", apiErr.Code)
			writeErr(w, http.StatusBadGateway, "sandboxd_error", "the sandbox backend failed to serve this request")
			return
		}
		writeErr(w, apiErr.Status, apiErr.Code, apiErr.Message)
		return
	}
	s.Log.Error("sandboxd unreachable", "error", err.Error())
	writeErr(w, http.StatusBadGateway, "sandboxd_unavailable", "the sandbox backend is unreachable")
}

// requireSandboxd answers 503 on the whole S3 surface when no backend
// is configured (dev/preview stacks without sandboxd).
func (s *Server) requireSandboxd(w http.ResponseWriter) bool {
	if s.Sandboxd == nil {
		writeErr(w, http.StatusServiceUnavailable, "sandboxd_unavailable",
			"sandbox backend not configured (PORTAL_SANDBOXD_URL)")
		return false
	}
	return true
}

// sandboxOwned loads the ownership row for id under org. Not-a-member
// of the owning org and unknown ids are the same 404 (plan 5: do not
// reveal existence). The row is the Portal's index; sandboxd stays the
// status authority.
func (s *Server) sandboxOwned(ctx context.Context, orgID, id string) (projectID string, ok bool) {
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(project_id::text, '') FROM sandboxes WHERE id = $1`, id,
		).Scan(&projectID)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		return "", false
	}
	return projectID, true
}

// syncSandboxRow upserts the local index row from the live sandboxd
// view (read-through sync). Best-effort: a failed sync logs and moves
// on — the next webhook or read repairs the drift.
func (s *Server) syncSandboxRow(ctx context.Context, orgID, projectID string, sb sandboxd.Sandbox) {
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO sandboxes
				(id, org_id, project_id, status, image, vcpu, memory_mb, ports, error_message, created_at, updated_at, stopped_at)
			VALUES ($1, $2::uuid, NULLIF($3,'')::uuid, $4, $5, $6, $7, $8, NULLIF($9,''), $10, now(),
			        CASE WHEN $4 IN ('stopped','exited') THEN now() ELSE NULL END)
			ON CONFLICT (id) DO UPDATE SET
				status        = EXCLUDED.status,
				image         = EXCLUDED.image,
				vcpu          = EXCLUDED.vcpu,
				memory_mb     = EXCLUDED.memory_mb,
				ports         = EXCLUDED.ports,
				error_message = EXCLUDED.error_message,
				updated_at    = now(),
				stopped_at    = CASE
					WHEN EXCLUDED.status IN ('stopped','exited') THEN COALESCE(sandboxes.stopped_at, now())
					ELSE NULL
				END`,
			sb.ID, orgID, projectID, sb.Status, sb.Image,
			sb.Resources.CPUs, sb.Resources.MemoryMB, sb.Ports, sb.ErrorMessage,
			parseRFC3339OrNow(sb.CreatedAt))
		return err
	})
	if err != nil {
		s.Log.Warn("sandbox index sync failed", "sandbox_id", sb.ID, "error", err.Error())
	}
}

// dropSandboxRow removes a purged sandbox from the index (webhook
// `deleted`, or a 404 from sandboxd during a read-through).
func (s *Server) dropSandboxRow(ctx context.Context, orgID, id string) {
	if err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM sandboxes WHERE id = $1`, id)
		return err
	}); err != nil {
		s.Log.Warn("sandbox index delete failed", "sandbox_id", id, "error", err.Error())
	}
}

func parseRFC3339OrNow(s string) time.Time {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t
	}
	return time.Now()
}

// auditSandbox writes a best-effort audit entry; failures never fail
// the request (same policy as the S2 key audit).
func (s *Server) auditSandbox(ctx context.Context, orgID, actorType, actorID, action, sandboxID string, extra map[string]any) {
	if extra == nil {
		extra = map[string]any{}
	}
	extra["sandbox_id"] = sandboxID
	meta, _ := json.Marshal(extra)
	if err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		return tenants.Audit(ctx, tx, actorType, actorID, action, sandboxID, string(meta))
	}); err != nil {
		s.Log.Warn("audit sandbox."+action+" failed", "error", err.Error())
	}
}

// --- idempotency (plan 5: Idempotency-Key on POST /v1/sandboxes + exec,
// stored 24h) ---------------------------------------------------------

// errIDConflict marks a concurrent duplicate key (caller answers 409).
var errIDConflict = errors.New("idempotency: in progress")

// idempotent runs fn once per (org, key); a replayed key with the same
// request hash returns the stored response, with a foreign hash a 422.
// fn returns (status, wireBody) and is executed exactly once per key —
// except after a failed attempt, which clears the claim so the client
// can retry.
func (s *Server) idempotent(ctx context.Context, orgID, key, requestHash string, fn func() (int, []byte, error)) (int, []byte, error) {
	if key == "" {
		status, body, err := fn()
		return status, body, err
	}
	if len(key) > 200 {
		return 0, nil, fmt.Errorf("Idempotency-Key too long")
	}
	var (
		replayStatus int
		replayBody   []byte
		replay       bool
		conflict     bool
	)
	err := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		// Lazy prune + claim. The ON CONFLICT DO NOTHING claim makes
		// concurrent duplicates deterministic: exactly one wins.
		tag, err := tx.Exec(ctx, `
			DELETE FROM idempotency_keys WHERE created_at < now() - interval '24 hours'`)
		_ = tag
		if err != nil {
			return err
		}
		tag, err = tx.Exec(ctx, `
			INSERT INTO idempotency_keys (org_id, key, request_hash)
			VALUES ($1::uuid, $2, $3)
			ON CONFLICT (org_id, key) DO NOTHING`, orgID, key, requestHash)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 1 {
			return nil // we own the claim; run fn
		}
		var (
			hash     string
			status   *int
			body     []byte
			claimAge time.Time
		)
		if err := tx.QueryRow(ctx, `
			SELECT request_hash, response_status, response_body, created_at
			FROM idempotency_keys WHERE org_id = $1::uuid AND key = $2`,
			orgID, key).Scan(&hash, &status, &body, &claimAge); err != nil {
			return err
		}
		if hash != requestHash {
			return errKeyReuse
		}
		if status != nil {
			replay, replayStatus, replayBody = true, *status, body
			return nil
		}
		// Claimed but unfinished. Young claims are another request in
		// flight; stale ones (crashed handler) are safe to take over.
		if time.Since(claimAge) < 2*time.Minute {
			conflict = true
			return nil
		}
		if _, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET request_hash = $3, created_at = now()
			WHERE org_id = $1::uuid AND key = $2`, orgID, key, requestHash); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if replay {
		return replayStatus, replayBody, nil
	}
	if conflict {
		return 0, nil, errIDConflict
	}

	status, body, fnErr := fn()
	if fnErr != nil {
		// Release the claim so a corrected retry can go through.
		if delErr := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM idempotency_keys WHERE org_id = $1::uuid AND key = $2`, orgID, key)
			return err
		}); delErr != nil {
			s.Log.Warn("idempotency claim release failed", "error", delErr.Error())
		}
		return 0, nil, fnErr
	}
	if storeErr := db.WithOrg(ctx, s.Pool, orgID, func(tx db.Tx) error {
		_, err := tx.Exec(ctx, `
			UPDATE idempotency_keys SET response_status = $3, response_body = $4
			WHERE org_id = $1::uuid AND key = $2`, orgID, key, status, body)
		return err
	}); storeErr != nil {
		s.Log.Warn("idempotency response store failed", "error", storeErr.Error())
	}
	return status, body, nil
}

var errKeyReuse = errors.New("idempotency: key reuse with a different body")

// requestHash fingerprints the request a key must replay.
func requestHash(method, path string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte{0})
	h.Write([]byte(path))
	h.Write([]byte{0})
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// --- small shared helpers -------------------------------------------

// parseIntDefault parses q or returns def on absence/error.
func parseIntDefault(q string, def, max int) int {
	if q == "" {
		return def
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 {
		return def
	}
	return min(n, max)
}

// splitCmd turns the UI's single-line command into an argv. Quoting is
// POSIX-ish: spaces split, double quotes group. Enough for the debug
// exec box, honest about what it does.
func splitCmd(line string) ([]string, bool) {
	var out []string
	var cur strings.Builder
	inQuote := false
	seen := false
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			seen = true
		case r == ' ' && !inQuote:
			if cur.Len() > 0 || seen {
				out = append(out, cur.String())
				cur.Reset()
				seen = false
			}
		default:
			cur.WriteRune(r)
		}
	}
	if inQuote {
		return nil, false
	}
	if cur.Len() > 0 || seen {
		out = append(out, cur.String())
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}
