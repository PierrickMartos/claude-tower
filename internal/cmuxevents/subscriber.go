package cmuxevents

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Event struct {
	Seq        int64     `json:"seq"`
	Name       string    `json:"name"`
	Category   string    `json:"category"`
	OccurredAt time.Time `json:"occurred_at"`
	// WorkspaceID is the top-level field, present on agent hooks, workspace.*
	// and surface.* events alike (the payload copy is not). Use it to correlate
	// a session with the workspace whose closure ends it.
	WorkspaceID string  `json:"workspace_id"`
	Payload     Payload `json:"payload"`
}

type Payload struct {
	SessionID     string `json:"session_id"`
	Cwd           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
	Phase         string `json:"phase"`
	WorkspaceID   string `json:"workspace_id"`
	Kind          string `json:"kind"`   // surface.closed: terminal | browser | …
	Origin        string `json:"origin"` // surface.closed: tab_close | detach | …
}

// ClosesWorkspace reports whether this event means a workspace (cmux tab) and
// the agent sessions inside it have gone away. surface.closed also fires on
// "detach" (the pane moved, not closed) and for non-terminal surfaces, so only
// a terminal tab_close counts.
func (e Event) ClosesWorkspace() bool {
	switch e.Name {
	case "workspace.closed":
		return true
	case "surface.closed":
		return e.Payload.Kind == "terminal" && e.Payload.Origin == "tab_close"
	}
	return false
}

// Subscribe spawns `cmux events` and returns a channel of decoded agent events.
// Closes the channel when ctx is cancelled. Reconnects with backoff on subprocess exit.
func Subscribe(ctx context.Context) (<-chan Event, error) {
	cursorDir := filepath.Join(os.Getenv("HOME"), ".cache", "claude-tower")
	if err := os.MkdirAll(cursorDir, 0o755); err != nil {
		return nil, err
	}
	cursor := filepath.Join(cursorDir, "cursor")

	out := make(chan Event, 64)
	go func() {
		defer close(out)
		for ctx.Err() == nil {
			err := stream(ctx, cursor, out)
			if err != nil && !errors.Is(err, context.Canceled) {
				select {
				case <-time.After(2 * time.Second):
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

func stream(ctx context.Context, cursor string, out chan<- Event) error {
	cmd := exec.CommandContext(ctx, "cmux", "events", "--cursor-file", cursor)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = cmd.Wait() }()

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		var head struct {
			Type     string `json:"type"`
			Category string `json:"category"`
		}
		line := scanner.Bytes()
		if err := json.Unmarshal(line, &head); err != nil {
			continue
		}
		if head.Type != "event" {
			continue
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		switch {
		case head.Category == "agent":
			if ev.Payload.SessionID == "" {
				continue
			}
		case ev.ClosesWorkspace():
			if ev.WorkspaceID == "" {
				continue
			}
		default:
			continue
		}
		select {
		case out <- ev:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return scanner.Err()
}
