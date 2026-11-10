package kube

// SeedWorkspace seeds an empty workspace directory by running the same
// skeleton-copy + chown the docker seed container does — as a
// short-lived Kubernetes Job whose pod mounts the workspace via
// hostPath. This is the only piece of workspace provisioning that the
// docker executor ran through Docker; with it, the kubernetes executor
// never touches a container runtime at all.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// SeedImage is the sandbox base image the seed Job runs (the same
// SANDBOXD_IMAGE that carries /opt/sandbox-skel).
var seedCommand = []string{"bash", "-lc",
	`cp -aT /opt/sandbox-skel/. /target/ && chown -R sandbox:sandbox /target && chmod 755 /target`,
}

// SeedWorkspace seeds hostDir (a node path, visible to pods through the
// executor's data hostPath). Blocks until the Job's pod succeeds
// (timeout ~2 min) and deletes the Job either way.
func (e *Engine) SeedWorkspace(ctx context.Context, image, hostDir string) error {
	if image == "" {
		return errors.New("kube.SeedWorkspace: no seed image (SANDBOXD_IMAGE)")
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return err
	}
	name := "sandboxd-seed-" + hex.EncodeToString(suffix)
	job := map[string]any{
		"apiVersion": "batch/v1",
		"kind":       "Job",
		"metadata": map[string]any{
			"name":      name,
			"namespace": e.ns,
			"labels":    map[string]string{labelManaged: "true"},
		},
		"spec": map[string]any{
			"backoffLimit":            0,
			"ttlSecondsAfterFinished": 0,
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]string{
					labelManaged: "true", labelSandbox: name,
				}},
				"spec": map[string]any{
					"restartPolicy":                 "Never",
					"automountServiceAccountToken":  false,
					"terminationGracePeriodSeconds": 10,
					"containers": []any{map[string]any{
						"name":    "seed",
						"image":   image,
						"command": seedCommand,
						"securityContext": map[string]any{
							"runAsUser":                0,
							"runAsNonRoot":             false,
							"allowPrivilegeEscalation": false,
							"privileged":               false,
							"readOnlyRootFilesystem":   false,
							"seccompProfile":           map[string]any{"type": "RuntimeDefault"},
						},
						"volumeMounts": []any{map[string]any{
							"name": "target", "mountPath": "/target",
						}},
					}},
					"volumes": []any{map[string]any{
						"name":     "target",
						"hostPath": map[string]any{"path": filepath.Clean(hostDir), "type": "DirectoryOrCreate"},
					}},
				},
			},
		},
	}
	if err := e.cli.do(ctx, "POST", "/apis/batch/v1/namespaces/"+e.ns+"/jobs", job, nil); err != nil {
		return fmt.Errorf("kube seed: create job: %w", err)
	}
	defer func() {
		dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = e.cli.do(dctx, "DELETE", "/apis/batch/v1/namespaces/"+e.ns+"/jobs/"+name, nil, nil)
	}()

	deadline := time.Now().Add(2 * time.Minute)
	path := "/apis/batch/v1/namespaces/" + e.ns + "/jobs/" + name
	for {
		var status struct {
			Status struct {
				Succeeded int `json:"succeeded"`
				Failed    int `json:"failed"`
			} `json:"status"`
		}
		if err := e.cli.do(ctx, "GET", path, nil, &status); err != nil {
			return fmt.Errorf("kube seed: job status: %w", err)
		}
		switch {
		case status.Status.Succeeded > 0:
			return nil
		case status.Status.Failed > 0:
			return errors.New("kube seed: job failed (image pull, node pressure, or skel missing)")
		}
		if time.Now().After(deadline) {
			return errors.New("kube seed: job did not finish in 2m")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
