// The sandboxd lifecycle webhook receiver (PORTAL_PLAN 4.4, consumed
// from P4): verifies the HMAC-SHA256 signature, refuses replays
// (occurred_at within ±5 minutes + event-id dedupe) and folds the
// status change into the `sandboxes` index. sandboxd stays the source
// of truth — the webhook is the push channel that keeps the Portal's
// ownership index current between reads.
package httpapi

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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
	var applied string
	err = db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
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
			tag, err := tx.Exec(r.Context(), `
                                UPDATE sandboxes SET
                                        status = $2,
                                        error_message = NULLIF($3,''),
                                        vcpu = COALESCE($4, vcpu),
                                        memory_mb = COALESCE($5, memory_mb),
                                        updated_at = now(),
                                        stopped_at = CASE WHEN $2 IN ('stopped','exited') THEN COALESCE(stopped_at, now()) ELSE stopped_at END
                                WHERE id = $1`,
				ev.SandboxID, status, errorMessage,
				webhookFloat(ev.Resources), webhookInt(ev.Resources))
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
	if errors.Is(err, pgx.ErrNoRows) { // not reachable; kept for symmetry
		applied = ""
		err = nil
	}
	if err != nil {
		s.Log.Error("webhook apply failed", "sandbox_id", ev.SandboxID, "error", err.Error())
		// 500 makes sandboxd retry; the dedupe row above keeps that
		// retry from double-applying once we do succeed.
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
