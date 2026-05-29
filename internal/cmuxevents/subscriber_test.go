package cmuxevents

import (
	"encoding/json"
	"testing"
)

func TestClosesWorkspace(t *testing.T) {
	cases := []struct {
		name string
		ev   Event
		want bool
	}{
		{"workspace closed", Event{Name: "workspace.closed"}, true},
		{"terminal tab close", Event{Name: "surface.closed", Payload: Payload{Kind: "terminal", Origin: "tab_close"}}, true},
		{"browser tab close", Event{Name: "surface.closed", Payload: Payload{Kind: "browser", Origin: "tab_close"}}, false},
		{"terminal detach", Event{Name: "surface.closed", Payload: Payload{Kind: "terminal", Origin: "detach"}}, false},
		{"agent hook", Event{Name: "agent.hook.Stop", Category: "agent"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.ev.ClosesWorkspace(); got != c.want {
				t.Errorf("ClosesWorkspace() = %v, want %v", got, c.want)
			}
		})
	}
}

// The workspace id lives at the top level of the frame, not inside payload, on
// surface.closed events — EndWorkspace correlation depends on reading it there.
func TestEventDecodesTopLevelWorkspaceID(t *testing.T) {
	line := []byte(`{"type":"event","category":"surface","name":"surface.closed",` +
		`"payload":{"kind":"terminal","origin":"tab_close"},"workspace_id":"WS-123"}`)
	var ev Event
	if err := json.Unmarshal(line, &ev); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ev.WorkspaceID != "WS-123" {
		t.Errorf("WorkspaceID = %q, want WS-123", ev.WorkspaceID)
	}
	if !ev.ClosesWorkspace() {
		t.Error("ClosesWorkspace() = false for a terminal tab_close")
	}
}
