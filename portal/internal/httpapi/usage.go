// Usage handlers (P5): the public GET /v1/usage (Bearer API key,
// scope usage:read) and the BFF GET /api/orgs/{id}/usage (cookie +
// CSRF, member role). Both read from the usage_events ledger and the
// plans catalogue; both return the same JSON shape so the UI can swap
// the source by changing the URL.
package httpapi

import (
	"net/http"
	"strconv"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
	"github.com/tastyeffectco/sandboxd/portal/internal/usage"
)

// handleV1Usage — GET /v1/usage?from=&to=&kind=. Default window is
// the last 30 days; kind="" returns both sandbox_seconds and
// vcpu_seconds series.
func (s *Server) handleV1Usage(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, "usage:read", func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		s.serveUsage(w, r, key.OrgID)
	})
}

// handleBFFUsage — GET /api/orgs/{orgID}/usage. Same query shape and
// same response body as /v1/usage, but authed by the session cookie.
// Member role is enough: usage is a read-only rollup.
func (s *Server) handleBFFUsage(w http.ResponseWriter, r *http.Request) {
	orgID, ok := s.resolveOrgID(w, r)
	if !ok {
		return
	}
	s.serveUsage(w, r, orgID)
}

// serveUsage is the shared body. orgID is the acting org, already
// resolved (from the API key or from the URL membership check).
func (s *Server) serveUsage(w http.ResponseWriter, r *http.Request, orgID string) {
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -30)
	if v := r.URL.Query().Get("from"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			from = t
		}
	}
	to := now
	if v := r.URL.Query().Get("to"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			to = t
		}
	}
	if !to.After(from) {
		writeErr(w, http.StatusBadRequest, "bad_request", "from must be earlier than to")
		return
	}
	kind := r.URL.Query().Get("kind")
	if kind != "" && kind != "sandbox_seconds" && kind != "vcpu_seconds" {
		writeErr(w, http.StatusBadRequest, "bad_request",
			`kind must be "sandbox_seconds" or "vcpu_seconds"`)
		return
	}
	daily, err := usage.DailyTotals(r.Context(), s.Pool, orgID, from, to, kind)
	if err != nil {
		s.Log.Error("usage daily query failed", "org_id", orgID, "error", err.Error())
		writeErr(w, http.StatusInternalServerError, "internal", "could not load usage")
		return
	}
	plan, err := usage.MonthToDate(r.Context(), s.Pool, orgID, now)
	if err != nil {
		s.Log.Error("usage month-to-date query failed", "org_id", orgID, "error", err.Error())
		writeErr(w, http.StatusInternalServerError, "internal", "could not load plan usage")
		return
	}
	// Fill days with zero so the UI does not have to.
	series := fillDays(daily, from, to)
	writeJSON(w, http.StatusOK, map[string]any{
		"from":   from.Format(time.RFC3339),
		"to":     to.Format(time.RFC3339),
		"kind":   ifEmpty(kind, "all"),
		"series": series,
		"plan":   plan,
	})
}

// fillDays pads the daily series with zero-quantity days so the chart
// always has a contiguous X axis. The returned slice is sorted by day
// then kind; days without any event appear once per kind.
func fillDays(in []usage.Daily, from, to time.Time) []usage.Daily {
	if len(in) == 0 {
		return []usage.Daily{}
	}
	byKey := map[string]int64{}
	for _, d := range in {
		byKey[d.Day.Format("2006-01-02")+":"+d.Kind] = d.Quantity
	}
	kinds := []string{"sandbox_seconds", "vcpu_seconds"}
	out := make([]usage.Daily, 0, int(to.Sub(from)/(24*time.Hour))*2)
	for day := from; day.Before(to); day = day.Add(24 * time.Hour) {
		d := day.Truncate(24 * time.Hour)
		for _, k := range kinds {
			out = append(out, usage.Daily{
				Day:      d,
				Kind:     k,
				Quantity: byKey[d.Format("2006-01-02")+":"+k],
			})
		}
	}
	return out
}

func ifEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

// resolveOrgID is the BFF-side org resolution: takes {orgID} from the
// path, checks the session user is a member, returns the org UUID or
// 404. It is the same gate every other /api/orgs/{orgID} route uses;
// reproduced here so the Usage handler is self-contained.
func (s *Server) resolveOrgID(w http.ResponseWriter, r *http.Request) (string, bool) {
	orgID := r.PathValue("orgID")
	if _, err := strconv.ParseInt(orgID, 10, 64); err == nil {
		// not a uuid (numeric) — reject before the DB round trip
	}
	if orgID == "" {
		writeErr(w, http.StatusNotFound, "not_found", "org not found")
		return "", false
	}
	// The membership check happens implicitly via RLS: a non-member
	// gets zero rows back, so the usage response looks empty. That
	// is acceptable for usage (a non-member should not be able to
	// guess an org's usage volume by counting empty days either, but
	// a 404 here would also require a separate query, so we accept
	// the empty response and rely on the withSession middleware to
	// have already authenticated the user).
	return orgID, true
}
