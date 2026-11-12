// The sandboxd lifecycle webhook receiver (PORTAL_PLAN 4.4, consumed
// from P4): verifies the HMAC-SHA256 signature, refuses replays
// (occurred_at within ±5 minutes + event-id dedupe) and folds the
// status change into the `sandboxes` index. sandboxd stays the source
// of truth — the webhook is the push channel that keeps the Portal's
// ownership index current between reads.
package httpapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
)

// webhookMaxSkew is the anti-replay window (plan: timestamp ±5m). A
// captured body replayed later fails here even before the event-id
// dedupe sees it.
const webhookMaxSkew = 5 * time.Minute

// webhookEvent is sandboxd's signed lifecycle body
// (control-plane/internal/webhook.Event).
type webhookEvent struct {
	ID         string            `json:"id"`
	Event      string            `json:"event"`
	SandboxID  string            `json:"sandbox_id"`
	External   map[string]string `json:"external,omitempty"`
	OccurredAt string            `json:"occurred_at"`
	Resources  *struct {
		CPUs     float64 `json:"cpus"`
		MemoryMB int     `json:"memory_mb"`
	} `json:"resources,omitempty"`
}

// handleSandboxdWebhook — POST /api/webhooks/sandboxd. Registered only
// when PORTAL_SANDBOXD_WEBHOOK_SECRET is set; signature auth, not
// session auth (a server-to-server channel).
func (s *Server) handleSandboxdWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "unreadable body")
		return
	}

	// 1. Signature: "sha256=<hex hmac-sha256(secret, body)>", compared
	// in constant time. sandboxd signs the exact bytes it sends.
	sig := r.Header.Get("X-Sandboxd-Signature")
	mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
	mac.Write(body)
	want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(sig), []byte(want)) != 1 {
		writeErr(w, http.StatusUnauthorized, "invalid_signature", "webhook signature mismatch")
		return
	}

	// 2. Parse + freshness.
	var ev webhookEvent
	if err := json.Unmarshal(body, &ev); err != nil || ev.SandboxID == "" || ev.Event == "" {
		writeErr(w, http.StatusBadRequest, "bad_request", "webhook body must carry event + sandbox_id")
		return
	}
	occurred, err := time.Parse(time.RFC3339, ev.OccurredAt)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", "occurred_at must be RFC3339")
		return
	}
	if skew := time.Since(occurred); skew > webhookMaxSkew || skew < -webhookMaxSkew {
		writeErr(w, http.StatusBadRequest, "stale_event", "event older than the replay window")
		return
	}
	if ev.ID == "" {
		ev.ID = ev.Event + ":" + ev.SandboxID + ":" + strconv.FormatInt(occurred.UnixMilli(), 10)
	}

	// 3. Dedupe: the first delivery wins; sandboxd's retries (and an
	// attacker replaying a fresh capture within the signature window —
	// they cannot forge new events) are acknowledged idempotently.
	orgID := ev.External["workspace_id"]
	if orgID == "" {
		// Untagged event: nothing tenant-scoped to update (sandboxes
		// are always tagged by the gateway).
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	fresh := false
	err = db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		tag, err := tx.Exec(r.Context(), `
                        INSERT INTO webhook_events (event_id, event) VALUES ($1, $2)
                        ON CONFLICT (event_id) DO NOTHING`, ev.ID, ev.Event)
		if err != nil {
			return err
		}
		fresh = tag.RowsAffected() == 1
		if !fresh {
			return nil
		}
		_, err = tx.Exec(r.Context(), `
                        DELETE FROM webhook_events WHERE created_at < now() - interval '25 hours'`)
		return err
	})
	if err != nil {
		s.Log.Error("webhook dedupe failed", "error", err.Error())
		writeErr(w, http.StatusInternalServerError, "internal", "could not record webhook event")
		return
	}
	if !fresh {
		writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
		return
	}

	// 4. Apply to the index. `deleted` drops the row (the index is not
	// history — usage_events in P5 is); every other event re-reads the
	// live view from sandboxd so the row stores facts, not hints, and
	// falls back to the event-derived status when sandboxd has already
	// moved on (deleted between webhook and Get).
	//
	// P5: 'started' stamps started_at; 'stopped'/'deleted' bill a
	// sandbox_seconds + vcpu_seconds usage_events row for the elapsed
	// window (idempotent on source_event_id).
	var applied string
	err = db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		// P5 billing window: started_at opens it (create or start),
		// the first stop or delete while it is open closes it and
		// bills sandbox_seconds + vcpu_seconds. stopped_at IS NULL
		// marks an open window, so stop-then-delete bills once.
		var (
			startedAt, stoppedAt *time.Time
			vcpu                 float64
			projectID            string
		)
		if ev.Event == "stopped" || ev.Event == "deleted" {
			err := tx.QueryRow(r.Context(), `
                                SELECT started_at, stopped_at, vcpu, COALESCE(project_id::text, '')
                                FROM sandboxes WHERE id = $1`, ev.SandboxID).Scan(
				&startedAt, &stoppedAt, &vcpu, &projectID)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if startedAt != nil && stoppedAt == nil {
				if err := billWindow(r.Context(), tx, orgID, projectID, ev.SandboxID, ev.ID,
					*startedAt, occurred, vcpu); err != nil {
					return err
				}
			}
		}
		switch ev.Event {
		case "deleted":
			tag, err := tx.Exec(r.Context(), `DELETE FROM sandboxes WHERE id = $1`, ev.SandboxID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				applied = "deleted"
			}
			return nil
		case "created", "started", "stopped", "failed":
			status := map[string]string{
				"created": "created", "started": "running",
				"stopped": "stopped", "failed": "failed",
			}[ev.Event]
			errorMessage := ""
			if ev.Event == "failed" {
				errorMessage = "sandbox failed (see sandboxd logs)"
			}
			// created/started open a window (a create runs the
			// sandbox; the gateway insert may already have opened
			// it, hence COALESCE for created). stopped/failed close
			// it.
			tag, err := tx.Exec(r.Context(), `
                                UPDATE sandboxes SET
                                        status = $2,
                                        error_message = NULLIF($3,''),
                                        vcpu = COALESCE($4, vcpu),
                                        memory_mb = COALESCE($5, memory_mb),
                                        started_at = CASE
                                                WHEN $6 = 'started' THEN $7
                                                WHEN $6 = 'created' THEN COALESCE(started_at, $7)
                                                ELSE started_at END,
                                        stopped_at = CASE
                                                WHEN $6 IN ('created','started') THEN NULL
                                                WHEN $6 IN ('stopped','failed') THEN COALESCE(stopped_at, $7)
                                                ELSE stopped_at END,
                                        updated_at = now()
                                WHERE id = $1`,
				ev.SandboxID, status, errorMessage,
				webhookFloat(ev.Resources), webhookInt(ev.Resources), ev.Event, occurred)
			if err != nil {
				return err
			}
			if tag.RowsAffected() > 0 {
				applied = status
			}
			return nil
		default:
			return nil // unknown event type: recorded, not acted on
		}
	})
	if err != nil {
		s.Log.Error("webhook apply failed", "sandbox_id", ev.SandboxID, "error", err.Error())
		// 500 makes sandboxd retry. The dedupe row was committed in
		// its own tx above, so roll it back to let the retry apply.
		_ = db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
			_, derr := tx.Exec(r.Context(), `DELETE FROM webhook_events WHERE event_id = $1`, ev.ID)
			return derr
		})
		writeErr(w, http.StatusInternalServerError, "internal", "could not apply webhook event")
		return
	}
	if applied == "" {
		// Unknown sandbox (created outside the portal, org purged…):
		// acknowledge so sandboxd stops retrying.
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "applied", "event": ev.Event})
}

func webhookFloat(r *struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
}) *float64 {
	if r == nil {
		return nil
	}
	return &r.CPUs
}

func webhookInt(r *struct {
	CPUs     float64 `json:"cpus"`
	MemoryMB int     `json:"memory_mb"`
}) *int {
	if r == nil {
		return nil
	}
	return &r.MemoryMB
}

// billWindow writes the usage_events rows for one closed run window:
// sandbox_seconds, and vcpu_seconds (seconds × vCPUs, rounded) when the
// sandbox has a vCPU size. Both are keyed by the closing event id
// (vcpu with a ":vcpu" suffix), so a replayed event inserts nothing.
func billWindow(ctx context.Context, tx db.Tx, orgID, projectID, sandboxID, eventID string, start, end time.Time, vcpu float64) error {
	secs := int64(end.Sub(start).Seconds())
	if secs < 0 {
		secs = 0
	}
	rows := []struct {
		kind, source string
		qty          int64
	}{{"sandbox_seconds", eventID, secs}}
	if vcpu > 0 {
		rows = append(rows, struct {
			kind, source string
			qty          int64
		}{"vcpu_seconds", eventID + ":vcpu", int64(math.Round(float64(secs) * vcpu))})
	}
	for _, row := range rows {
		if _, err := tx.Exec(ctx, `
                        INSERT INTO usage_events
                                (org_id, project_id, sandbox_id, kind, quantity, unit, source_event_id, occurred_at)
                        VALUES ($1::uuid, NULLIF($2,'')::uuid, $3, $4, $5, 'seconds', $6, $7)
                        ON CONFLICT (source_event_id) DO NOTHING`,
			orgID, projectID, sandboxID, row.kind, row.qty, row.source, end); err != nil {
			return fmt.Errorf("insert usage_event: %w", err)
		}
	}
	return nil
}
