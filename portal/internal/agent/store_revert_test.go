// store_revert_test.go — A3.3 store layer (plan §8 + §11 "A3.3 Xong
// khi"): the revert guard matrix and the usage_events ledger write.
// Pure Postgres — no sandbox backend: PrepareRevert/ClaimRevert and
// SetFinal never touch the sandbox.
package agent

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
)

// TestRevertGuardMatrix — PrepareRevert/ClaimRevert answer exactly the
// plan §8 rules: terminal only, not yet reverted, checkpoint present,
// no active run on the sandbox, latest run only.
func TestRevertGuardMatrix(t *testing.T) {
	bundle := setupDB(t)
	st := NewStore(bundle.Pool)
	ctx := context.Background()

	orgID, _, sandboxID := seedOrg(t, st, "rev")

	mkRun := func(tag string) Run {
		t.Helper()
		run, _, err := st.CreateRun(ctx, CreateIn{
			OrgID: orgID, SandboxID: sandboxID, Prompt: "p " + tag,
			Model: "glm-4.6", MaxSteps: 5,
		})
		if err != nil {
			t.Fatalf("create run %s: %v", tag, err)
		}
		return run
	}
	finish := func(run Run) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{
			"summary": "s", "files_changed": []FileChange{{Path: "hello.txt", Status: "A"}},
			"steps": 2,
		})
		if err := st.SetFinal(ctx, orgID, run.ID, FinalState{
			Status: StatusSucceeded, Result: raw, SandboxID: run.SandboxID,
			Steps: 2, InputTokens: 30, OutputTokens: 12,
		}); err != nil {
			t.Fatalf("finish run: %v", err)
		}
	}

	// A queued run is active — the sandbox has a run in progress.
	active := mkRun("active")
	if _, err := st.PrepareRevert(ctx, orgID, active.ID); err != ErrRunInProgress {
		t.Fatalf("revert of a queued run = %v, want ErrRunInProgress", err)
	}

	// Terminal but without a checkpoint → no_checkpoint. (The migration
	// defaults checkpoint_ref to NULL; the loop only sets it on success.)
	finish(active)
	if _, err := st.PrepareRevert(ctx, orgID, active.ID); err != ErrNoCheckpoint {
		t.Fatalf("revert without checkpoint = %v, want ErrNoCheckpoint", err)
	}

	// Give it a checkpoint: now it is revertible.
	st.SetCheckpoint(ctx, orgID, active.ID, "refs/4aicoding/checkpoints/"+active.ID)
	got, err := st.PrepareRevert(ctx, orgID, active.ID)
	if err != nil {
		t.Fatalf("prepare revert: %v", err)
	}
	if got.ID != active.ID || got.CheckpointRef == "" {
		t.Fatalf("prepare revert returned %+v", got)
	}

	// A NEWER TERMINAL run makes the old one not_latest_run (a newer
	// ACTIVE run answers run_in_progress first — the guard matrix
	// order is: active run → latest run).
	newer := mkRun("newer")
	finish(newer)
	st.SetCheckpoint(ctx, orgID, newer.ID, "refs/4aicoding/checkpoints/"+newer.ID)
	if _, err := st.PrepareRevert(ctx, orgID, active.ID); err != ErrNotLatestRun {
		t.Fatalf("revert of an older run = %v, want ErrNotLatestRun", err)
	}

	// Claim once: true. Claim again: false. Prepare afterwards:
	// already_reverted.
	if ok, err := st.ClaimRevert(ctx, orgID, newer.ID); err != nil || !ok {
		t.Fatalf("first claim = %v, %v; want true", ok, err)
	}
	if ok, _ := st.ClaimRevert(ctx, orgID, newer.ID); ok {
		t.Fatal("second claim = true, want false")
	}
	if _, err := st.PrepareRevert(ctx, orgID, newer.ID); err != ErrAlreadyReverted {
		t.Fatalf("prepare after revert = %v, want ErrAlreadyReverted", err)
	}

	// UnclaimRevert makes it revertible again (a failed exec rolls back).
	st.UnclaimRevert(ctx, orgID, newer.ID)
	if _, err := st.PrepareRevert(ctx, orgID, newer.ID); err != nil {
		t.Fatalf("prepare after unclaim: %v", err)
	}

	// Foreign org → 404, never 403.
	otherOrg, _, _ := seedOrg(t, st, "rev-other")
	if _, err := st.PrepareRevert(ctx, otherOrg, newer.ID); err != ErrNotFound {
		t.Fatalf("cross-org revert = %v, want ErrNotFound", err)
	}
}

// TestUsageEventsOnFinish — SetFinal records the run into the P5
// usage ledger (plan §11 A3.3): one agent_tokens row (input+output,
// unit tokens) and one agent_steps row, idempotent under a re-claim.
func TestUsageEventsOnFinish(t *testing.T) {
	bundle := setupDB(t)
	st := NewStore(bundle.Pool)
	ctx := context.Background()

	orgID, _, sandboxID := seedOrg(t, st, "usage")
	run, _, err := st.CreateRun(ctx, CreateIn{
		OrgID: orgID, SandboxID: sandboxID, Prompt: "p", Model: "glm-4.6", MaxSteps: 5,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	raw, _ := json.Marshal(map[string]any{"summary": "s"})
	if err := st.SetFinal(ctx, orgID, run.ID, FinalState{
		Status: StatusSucceeded, Result: raw, SandboxID: sandboxID,
		Steps: 3, InputTokens: 200, OutputTokens: 100,
	}); err != nil {
		t.Fatalf("finish: %v", err)
	}
	// A second SetFinal must be a no-op (already final keeps the first
	// outcome — the usage rows too).
	_ = st.SetFinal(ctx, orgID, run.ID, FinalState{
		Status: StatusFailed, ErrorCode: ErrInternal, SandboxID: sandboxID,
		Steps: 9, InputTokens: 999, OutputTokens: 999,
	})

	type row struct {
		Kind     string
		Quantity int64
		Unit     string
	}
	rows := []row{}
	if err := db.WithOrg(ctx, st.Pool, orgID, func(tx db.Tx) error {
		rs, err := tx.Query(ctx, `
			SELECT kind, quantity, unit FROM usage_events
			WHERE sandbox_id = $1 AND kind LIKE 'agent_%'
			ORDER BY kind`, sandboxID)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.Kind, &r.Quantity, &r.Unit); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return rs.Err()
	}); err != nil {
		t.Fatalf("query usage_events: %v", err)
	}
	want := []row{
		{"agent_steps", 3, "steps"},
		{"agent_tokens", 300, "tokens"},
	}
	if len(rows) != len(want) {
		t.Fatalf("usage rows = %v, want %v", rows, want)
	}
	for i, w := range want {
		if rows[i] != w {
			t.Errorf("usage row %d = %+v, want %+v", i, rows[i], w)
		}
	}

	// The reverted event carries counts (plan §9.3) — store level.
	if err := st.AppendRevertEvent(ctx, orgID, run.ID, 1, 2, nil); err != nil {
		t.Fatalf("append reverted event: %v", err)
	}
	events, err := st.EventsSince(ctx, orgID, run.ID, 0)
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	var found map[string]any
	for _, e := range events {
		if e.Type == "reverted" {
			found = map[string]any{}
			if err := json.Unmarshal(e.Data, &found); err != nil {
				t.Fatalf("decode reverted event: %v", err)
			}
		}
	}
	if found == nil {
		t.Fatal("no reverted event recorded")
	}
	if found["files_restored"].(float64) != 1 || found["files_removed"].(float64) != 2 {
		t.Fatalf("reverted event counts = %v", found)
	}
	if fs, ok := found["files_remaining"].([]any); !ok || len(fs) != 0 {
		t.Fatalf("files_remaining = %v, want []", found["files_remaining"])
	}

	// RevertedAt flows through scanRun (claim first — the event alone
	// does not set the column).
	if ok, err := st.ClaimRevert(ctx, orgID, run.ID); err != nil || !ok {
		t.Fatalf("claim revert = %v, %v; want true", ok, err)
	}
	run2, err := st.GetRun(ctx, orgID, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run2.RevertedAt.Unix() <= 0 || run2.RevertedAt.After(time.Now().Add(time.Minute)) {
		t.Fatalf("RevertedAt = %v, want a real timestamp", run2.RevertedAt)
	}
}
