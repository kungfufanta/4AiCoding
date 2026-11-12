// A1 — Portal gateway for the workspace files API (AGENT_PLAN A0/A1).
// The five routes mirror sandboxd's /v1/sandboxes/{id}/files* surface;
// the Portal enforces ownership (keyOwnsSandbox → 404 on chéo org/project,
// matching P4) and the Portal scope `sandboxes:files` (already in the
// KnownScopes list, see apikeys/apikeys.go), then proxies the request
// to sandboxd byte-for-byte. Responses are relayed verbatim: the JSON
// shape (entries[], path/encoding/data, {from,to}) is sandboxd's
// contract; the Portal does not re-encode it.
//
// Path-safety is enforced by sandboxd (noSymlinkComponents, safeJoin,
// realpathWithin — A0 review #47); the Portal does not validate paths
// itself, both because the sandboxd layer is the canonical guard and
// because re-implementing it here would diverge from the upstream fix.
//
// Per-call timeouts: file reads up to 2 MiB take <1s in practice, so
// the per-call HTTP client (c.httpClient, 30s default) is fine. PUT
// bodies are capped at 8 MiB by sandboxd anyway (maxPutFileBytes);
// the Portal caps the request body at the same value via
// http.MaxBytesReader so a hostile caller cannot stream forever.
package httpapi

import (
	"io"
	"net/http"
	"net/url"
	"strconv"

	"github.com/tastyeffectco/sandboxd/portal/internal/apikeys"
)

// Portal-side scope for the files gateway (already in KnownScopes).
const portalScopeFiles = "sandboxes:files"

// maxPortalFileBodyBytes mirrors sandboxd's maxPutFileBytes (8 MiB);
// the Portal caps the request body so a hostile caller cannot stream
// forever into Portal memory before sandboxd rejects.
const maxPortalFileBodyBytes int64 = 8 << 20

// handleV1ListFiles — GET /v1/sandboxes/{id}/files.
func (s *Server) handleV1ListFiles(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, portalScopeFiles, func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		id := r.PathValue("id")
		if !s.keyOwnsSandbox(r.Context(), key, id) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		s.proxyFiles(w, r, id, http.MethodGet, "/files", nil, "")
	})
}

// handleV1FileContent — GET /v1/sandboxes/{id}/files/content.
func (s *Server) handleV1FileContent(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, portalScopeFiles, func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		id := r.PathValue("id")
		if !s.keyOwnsSandbox(r.Context(), key, id) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		s.proxyFiles(w, r, id, http.MethodGet, "/files/content", nil, "")
	})
}

// handleV1PutFile — PUT /v1/sandboxes/{id}/files/content.
func (s *Server) handleV1PutFile(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, portalScopeFiles, func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		id := r.PathValue("id")
		if !s.keyOwnsSandbox(r.Context(), key, id) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		// Read the body up-front so we can forward it as a byte slice
		// (sandboxd's DisallowUnknownFields does not care about the
		// content-type; the body is the raw file bytes, not JSON).
		r.Body = http.MaxBytesReader(w, r.Body, maxPortalFileBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large",
				"file body exceeds the 8 MiB cap")
			return
		}
		if !s.proxyFiles(w, r, id, http.MethodPut, "/files/content", body, "application/octet-stream") {
			return
		}
		s.auditSandbox(r.Context(), key.OrgID, "api_key", key.KeyID, "sandbox.file.write", id,
			map[string]any{"path": r.URL.Query().Get("path"), "bytes": len(body)})
	})
}

// handleV1DeleteFile — DELETE /v1/sandboxes/{id}/files.
func (s *Server) handleV1DeleteFile(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, portalScopeFiles, func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		id := r.PathValue("id")
		if !s.keyOwnsSandbox(r.Context(), key, id) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		if !s.proxyFiles(w, r, id, http.MethodDelete, "/files", nil, "") {
			return
		}
		s.auditSandbox(r.Context(), key.OrgID, "api_key", key.KeyID, "sandbox.file.delete", id,
			map[string]any{"path": r.URL.Query().Get("path")})
	})
}

// handleV1MoveFile — POST /v1/sandboxes/{id}/files/move.
func (s *Server) handleV1MoveFile(w http.ResponseWriter, r *http.Request) {
	s.withAPIKey(w, r, portalScopeFiles, func(w http.ResponseWriter, r *http.Request, key apikeys.Key) {
		if !s.requireSandboxd(w) {
			return
		}
		id := r.PathValue("id")
		if !s.keyOwnsSandbox(r.Context(), key, id) {
			writeErr(w, http.StatusNotFound, "not_found", "sandbox not found")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 65536)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large",
				"move body exceeds the 64 KiB cap")
			return
		}
		if !s.proxyFiles(w, r, id, http.MethodPost, "/files/move", body, "application/json") {
			return
		}
		s.auditSandbox(r.Context(), key.OrgID, "api_key", key.KeyID, "sandbox.file.move", id, nil)
	})
}

// proxyFiles forwards a files API call to sandboxd. The query string is
// preserved verbatim; the body (when present) is forwarded as bytes.
// The response body is streamed byte-for-byte to the caller so a 2 MiB
// read does not buffer twice. Errors from sandboxd are decoded into
// the shared APIError envelope (status preserved) so a 400/404 from
// sandboxd surfaces with the same code the caller would see directly.
func (s *Server) proxyFiles(w http.ResponseWriter, r *http.Request, id, method, suffix string, body []byte, contentType string) bool {
	path := "/v1/sandboxes/" + url.PathEscape(id) + suffix
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	res, err := s.Sandboxd.Raw(r.Context(), method, path, body, contentType)
	if err != nil {
		s.writeSandboxdErr(w, err)
		return false
	}
	defer res.Body.Close()
	// Relay the headers sandboxd set (Content-Type for JSON responses;
	// none of the files routes set Cache-Control, but forward it if set).
	for _, h := range []string{"Content-Type", "Cache-Control"} {
		if v := res.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(res.StatusCode)
	// Cap the relay at 16 MiB so a misbehaving sandboxd cannot stream
	// forever into Portal memory (sandboxd caps reads at 2 MiB; 16 is
	// a generous safety margin for listings of large directories).
	if _, err := io.Copy(w, io.LimitReader(res.Body, 16<<20)); err != nil {
		// The client may have disconnected mid-stream; nothing to do.
		_ = err
	}
	return true
}

// itoa is a tiny helper for the audit metadata; not exported.
func itoa(n int) string { return strconv.Itoa(n) }
