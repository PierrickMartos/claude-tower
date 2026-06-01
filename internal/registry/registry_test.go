package registry_test

import (
	"testing"
	"time"

	"claude-tower/internal/cmuxevents"
	"claude-tower/internal/registry"
)

func evt(sid, hook string) cmuxevents.Event {
	return cmuxevents.Event{
		OccurredAt: time.Unix(1000, 0),
		Payload: cmuxevents.Payload{
			SessionID:     sid,
			HookEventName: hook,
		},
	}
}

func TestApplyStripsClaudePrefix(t *testing.T) {
	r := registry.New()
	s := r.Apply(evt("claude-abc123", "PreToolUse"))
	if s == nil {
		t.Fatal("expected a session, got nil")
	}
	if s.ID != "abc123" {
		t.Errorf("session ID = %q, want %q (claude- prefix must be stripped)", s.ID, "abc123")
	}
}

func TestApplyEmptySessionReturnsNil(t *testing.T) {
	r := registry.New()
	if s := r.Apply(evt("", "PreToolUse")); s != nil {
		t.Errorf("Apply with empty session id = %+v, want nil", s)
	}
	// A bare "claude-" prefix normalizes to empty and must also be ignored.
	if s := r.Apply(evt("claude-", "PreToolUse")); s != nil {
		t.Errorf("Apply with claude- only = %+v, want nil", s)
	}
}

func TestApplyStatusTransitions(t *testing.T) {
	cases := []struct {
		hook string
		want registry.Status
	}{
		{"PreToolUse", registry.StatusRunning},
		{"PostToolUse", registry.StatusRunning},
		{"UserPromptSubmit", registry.StatusRunning},
		{"Notification", registry.StatusAwaiting},
		{"PermissionRequest", registry.StatusAwaiting},
		{"Stop", registry.StatusIdle},
		{"SubagentStop", registry.StatusIdle},
		{"SessionEnd", registry.StatusEnded},
	}
	for _, c := range cases {
		t.Run(c.hook, func(t *testing.T) {
			r := registry.New()
			s := r.Apply(evt("sid", c.hook))
			if s.Status != c.want {
				t.Errorf("hook %q → status %v, want %v", c.hook, s.Status, c.want)
			}
		})
	}
}

func TestApplyUnknownHookLeavesStatusUntouched(t *testing.T) {
	r := registry.New()
	r.Apply(evt("sid", "PreToolUse")) // running
	s := r.Apply(evt("sid", "MysteryHook"))
	if s.Status != registry.StatusRunning {
		t.Errorf("unknown hook changed status to %v, want it to remain running", s.Status)
	}
}

func TestApplyPopulatesFields(t *testing.T) {
	r := registry.New()
	e := cmuxevents.Event{
		OccurredAt: time.Unix(2000, 0),
		Payload: cmuxevents.Payload{
			SessionID:     "sid",
			Cwd:           "/Users/me/work/my-app",
			HookEventName: "PreToolUse",
			ToolName:      "Bash",
		},
	}
	s := r.Apply(e)
	if s.Cwd != "/Users/me/work/my-app" {
		t.Errorf("Cwd = %q", s.Cwd)
	}
	if s.ProjectDir != "my-app" {
		t.Errorf("ProjectDir = %q, want base of cwd", s.ProjectDir)
	}
	if s.LastTool != "Bash" {
		t.Errorf("LastTool = %q", s.LastTool)
	}
	if s.LastHook != "PreToolUse" {
		t.Errorf("LastHook = %q", s.LastHook)
	}
	if !s.LastEvent.Equal(time.Unix(2000, 0)) {
		t.Errorf("LastEvent = %v", s.LastEvent)
	}
	if !s.Dirty {
		t.Error("Dirty = false, want true after Apply")
	}
}

func TestApplyEmptyFieldsDoNotClobber(t *testing.T) {
	r := registry.New()
	r.Apply(cmuxevents.Event{Payload: cmuxevents.Payload{
		SessionID: "sid", Cwd: "/a/proj", ToolName: "Edit", HookEventName: "PreToolUse",
	}})
	// A later event with empty cwd/tool must not wipe the previously-set values.
	s := r.Apply(cmuxevents.Event{Payload: cmuxevents.Payload{
		SessionID: "sid", HookEventName: "Stop",
	}})
	if s.Cwd != "/a/proj" || s.LastTool != "Edit" {
		t.Errorf("empty fields clobbered state: cwd=%q tool=%q", s.Cwd, s.LastTool)
	}
}

func TestEndWorkspaceEndsMatchingSessionsOnly(t *testing.T) {
	r := registry.New()
	r.Apply(cmuxevents.Event{WorkspaceID: "ws1", OccurredAt: time.Unix(100, 0),
		Payload: cmuxevents.Payload{SessionID: "a", HookEventName: "PreToolUse"}})
	r.Apply(cmuxevents.Event{WorkspaceID: "ws2", OccurredAt: time.Unix(100, 0),
		Payload: cmuxevents.Payload{SessionID: "b", HookEventName: "PreToolUse"}})

	r.EndWorkspace("ws1")

	snap := r.Snapshot()
	if len(snap) != 1 || snap[0].ID != "b" {
		t.Fatalf("snapshot = %+v, want only session b active (ws1 ended)", snap)
	}
}

func TestEndWorkspaceEmptyIDIsNoOp(t *testing.T) {
	r := registry.New()
	// A session with no reported workspace id (e.g. bootstrapped from a transcript)
	// must not be swept away by a stray empty-id close.
	r.Apply(evt("a", "PreToolUse"))
	r.EndWorkspace("")
	if len(r.Snapshot()) != 1 {
		t.Error(`EndWorkspace("") ended a session with empty workspace id`)
	}
}

func TestSetSummaryClearsDirty(t *testing.T) {
	r := registry.New()
	r.Apply(evt("sid", "PreToolUse")) // Dirty = true
	r.SetSummary("sid", "fixing flaky test")

	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot len = %d, want 1", len(snap))
	}
	if snap[0].Summary != "fixing flaky test" {
		t.Errorf("Summary = %q", snap[0].Summary)
	}
	if snap[0].Dirty {
		t.Error("Dirty = true, want false after SetSummary")
	}
}

func TestSetSummaryUnknownSessionIsNoOp(t *testing.T) {
	r := registry.New()
	r.SetSummary("ghost", "x") // must not panic or create a session
	if len(r.Snapshot()) != 0 {
		t.Error("SetSummary on unknown id created a session")
	}
}

func TestSnapshotFiltersEndedAndSortsRecentFirst(t *testing.T) {
	r := registry.New()
	r.Apply(cmuxevents.Event{OccurredAt: time.Unix(100, 0), Payload: cmuxevents.Payload{SessionID: "old", HookEventName: "PreToolUse"}})
	r.Apply(cmuxevents.Event{OccurredAt: time.Unix(300, 0), Payload: cmuxevents.Payload{SessionID: "new", HookEventName: "PreToolUse"}})
	r.Apply(cmuxevents.Event{OccurredAt: time.Unix(200, 0), Payload: cmuxevents.Payload{SessionID: "gone", HookEventName: "SessionEnd"}})

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len = %d, want 2 (ended session filtered out)", len(snap))
	}
	if snap[0].ID != "new" || snap[1].ID != "old" {
		t.Errorf("order = [%s, %s], want most-recent-first [new, old]", snap[0].ID, snap[1].ID)
	}
}

func TestSnapshotReturnsCopies(t *testing.T) {
	r := registry.New()
	r.Apply(evt("sid", "PreToolUse"))
	snap := r.Snapshot()
	snap[0].Summary = "mutated copy"
	// Mutating the snapshot must not leak back into the registry.
	if again := r.Snapshot(); again[0].Summary != "" {
		t.Errorf("Snapshot returned a live pointer; registry Summary = %q", again[0].Summary)
	}
}

func TestBootstrapSessionSeedsIdleThenNoOps(t *testing.T) {
	r := registry.New()
	s := r.BootstrapSession("sid", "/x/proj", "Read", time.Unix(500, 0))
	if s == nil {
		t.Fatal("first BootstrapSession returned nil")
	}
	if s.Status != registry.StatusIdle || s.ProjectDir != "proj" || s.LastTool != "Read" {
		t.Errorf("seeded session = %+v", s)
	}
	if dup := r.BootstrapSession("sid", "/other", "Bash", time.Unix(900, 0)); dup != nil {
		t.Errorf("second BootstrapSession for same id = %+v, want nil (no-op)", dup)
	}
	// Original state must be untouched by the no-op call.
	if snap := r.Snapshot(); snap[0].Cwd != "/x/proj" {
		t.Errorf("no-op bootstrap overwrote cwd = %q", snap[0].Cwd)
	}
}
