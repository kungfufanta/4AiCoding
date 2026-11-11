// webhook.go — Portal S3a: deliver sandbox lifecycle events to the
// configured SANDBOXD_WEBHOOK_URL (see internal/webhook). Best-effort by
// contract: a nil dispatcher, a marshal problem or a receiver outage never
// affects the API request that produced the event.
package api

import (
	"net/http"

	"github.com/tastyeffectco/sandboxd/control-plane/internal/store"
	"github.com/tastyeffectco/sandboxd/control-plane/internal/webhook"
)

// emitLifecycle delivers one lifecycle webhook for a sandbox. The payload's
// external + resources come from the stored row (nil row = omitted fields —
// used for the `failed` event where the row may not reflect the spec).
//
// The event fires on the node that executes the transition: locally for
// single-node installs and the control plane's own sandboxes; on the
// executor worker for fleet-forwarded transitions (the worker runs this
// same handler). Configure SANDBOXD_WEBHOOK_URL where the Portal should
// hear from.
func (s *Server) emitLifecycle(r *http.Request, event, sandboxID string, sb *store.Sandbox) {
	if !s.Webhook.Enabled() {
		return
	}
	ev := webhook.Event{Event: event, SandboxID: sandboxID}
	if sb != nil {
		ev.External = map[string]string{}
		if sb.ExternalUserID.Valid {
			ev.External["user_id"] = sb.ExternalUserID.String
		}
		if sb.ExternalProjectID.Valid {
			ev.External["project_id"] = sb.ExternalProjectID.String
		}
		if sb.ExternalWorkspaceID.Valid {
			ev.External["workspace_id"] = sb.ExternalWorkspaceID.String
		}
		ev.Resources = &webhook.Resources{CPUs: sb.Spec.CPUs, MemoryMB: s.effectiveMemoryMB(sb)}
		if len(ev.External) == 0 {
			ev.External = nil
		}
	}
	s.Webhook.Dispatch(ev)
}
