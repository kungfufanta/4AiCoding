package api

import (
	"path/filepath"
	"runtime"
	"testing"
)

// requiredPublicSurface is the public /v1 API the console + SDK depend on. The
// parity test (TestOpenAPIContractMatchesRoutes) keeps routes and the spec in
// sync, but parity alone wouldn't notice a route AND its spec entry being
// deleted together. This test pins the surface so a removal fails loudly.
var requiredPublicSurface = []string{
	"GET /v1/settings",
	"PATCH /v1/settings",
	"GET /v1/sandboxes",
	"POST /v1/sandboxes",
	"GET /v1/sandboxes/{id}",
	"POST /v1/sandboxes/{id}/start",
	"POST /v1/sandboxes/{id}/stop",
	"POST /v1/sandboxes/{id}/restart",
	"POST /v1/sandboxes/{id}/exec",
	"GET /v1/sandboxes/{id}/logs",
	"DELETE /v1/sandboxes/{id}",
	"GET /v1/sandboxes/{id}/files",
	"GET /v1/sandboxes/{id}/files/content",
	"PUT /v1/sandboxes/{id}/files/content",
	"DELETE /v1/sandboxes/{id}/files",
	"POST /v1/sandboxes/{id}/files/move",
	"POST /v1/snapshots",
	"GET /v1/snapshots",
	"GET /v1/snapshots/{id}",
	"DELETE /v1/snapshots/{id}",
}

func TestPublicAPISurfacePresent(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	dir := filepath.Dir(thisFile)
	routes := v1RoutesFromSource(t, filepath.Join(dir, "api.go"))
	spec := v1OpsFromSpec(t, filepath.Join(dir, "..", "..", "..", "docs", "openapi.yaml"))

	for _, op := range requiredPublicSurface {
		if !routes[op] {
			t.Errorf("required public route missing from api.go: %s", op)
		}
		if !spec[op] {
			t.Errorf("required public route missing from docs/openapi.yaml: %s", op)
		}
	}
}
