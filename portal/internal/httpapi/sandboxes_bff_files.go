package httpapi

import "net/http"

// Read-only workspace files for the Chat "Code" tab (session BFF). The
// same sandboxd /files surface as the /v1 gateway, gated by org
// ownership of the sandbox instead of an API key scope. Path safety
// (no escape, no symlinks, reserved dirs) is enforced by sandboxd.

// handleBFFListFiles — GET /api/orgs/{orgID}/sandboxes/{id}/files.
func (s *Server) handleBFFListFiles(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	id := r.PathValue("id")
	if _, ok := s.requireOwnedSandbox(w, r, orgID, id); !ok {
		return
	}
	if !s.requireSandboxd(w) {
		return
	}
	s.proxyFiles(w, r, id, http.MethodGet, "/files", nil, "")
}

// handleBFFFileContent — GET /api/orgs/{orgID}/sandboxes/{id}/files/content.
func (s *Server) handleBFFFileContent(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	id := r.PathValue("id")
	if _, ok := s.requireOwnedSandbox(w, r, orgID, id); !ok {
		return
	}
	if !s.requireSandboxd(w) {
		return
	}
	s.proxyFiles(w, r, id, http.MethodGet, "/files/content", nil, "")
}
