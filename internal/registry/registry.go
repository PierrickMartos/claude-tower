package registry

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"claude-tower/internal/cmuxevents"
)

type Status int

const (
	StatusIdle Status = iota
	StatusRunning
	StatusAwaiting
	StatusEnded
)

var (
	statusName  = [...]string{"idle", "running", "awaiting", "ended"}
	statusGlyph = [...]string{"○", "●", "◐", "·"}
)

func (s Status) String() string { return statusName[s] }
func (s Status) Glyph() string  { return statusGlyph[s] }

type Session struct {
	ID          string
	WorkspaceID string
	Cwd         string
	ProjectDir  string
	Status      Status
	LastTool    string
	LastHook    string
	LastEvent   time.Time
	Summary     string
	SummaryAt   time.Time
	Dirty       bool
}

type Registry struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func New() *Registry {
	return &Registry{sessions: map[string]*Session{}}
}

// Apply updates registry state from an event and returns the session.
// Returns nil for events without a session id.
func (r *Registry) Apply(e cmuxevents.Event) *Session {
	sid := strings.TrimPrefix(e.Payload.SessionID, "claude-")
	if sid == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.sessions[sid]
	if !ok {
		s = &Session{ID: sid}
		r.sessions[sid] = s
	}
	if e.WorkspaceID != "" {
		s.WorkspaceID = e.WorkspaceID
	}
	if e.Payload.Cwd != "" {
		s.Cwd = e.Payload.Cwd
		s.ProjectDir = filepath.Base(e.Payload.Cwd)
	}
	if !e.OccurredAt.IsZero() {
		s.LastEvent = e.OccurredAt
	}
	if e.Payload.HookEventName != "" {
		s.LastHook = e.Payload.HookEventName
	}
	if e.Payload.ToolName != "" {
		s.LastTool = e.Payload.ToolName
	}
	s.Dirty = true

	switch e.Payload.HookEventName {
	case "PreToolUse", "PostToolUse", "UserPromptSubmit":
		s.Status = StatusRunning
	case "Notification", "PermissionRequest":
		s.Status = StatusAwaiting
	case "Stop", "SubagentStop":
		s.Status = StatusIdle
	case "SessionEnd":
		s.Status = StatusEnded
	}
	return s
}

// BootstrapSession seeds an idle session at startup before any events arrive.
// No-op if the session is already known.
func (r *Registry) BootstrapSession(id, cwd, lastTool string, lastEvent time.Time) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.sessions[id]; exists {
		return nil
	}
	s := &Session{
		ID:         id,
		Cwd:        cwd,
		ProjectDir: filepath.Base(cwd),
		Status:     StatusIdle,
		LastTool:   lastTool,
		LastEvent:  lastEvent,
		Dirty:      true,
	}
	r.sessions[id] = s
	return s
}

// EndWorkspace marks every session belonging to a cmux workspace as ended.
// cmux only fires a graceful SessionEnd hook on /exit-style quits; closing the
// tab/terminal kills the process abruptly, so this is the only signal that the
// session is gone. No-op for an empty id so we never mass-end sessions that
// haven't yet reported a workspace (e.g. bootstrapped from a transcript).
func (r *Registry) EndWorkspace(workspaceID string) {
	if workspaceID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.sessions {
		if s.WorkspaceID == workspaceID {
			s.Status = StatusEnded
		}
	}
}

func (r *Registry) SetSummary(id, summary string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[id]
	if !ok {
		return
	}
	s.Summary = summary
	s.SummaryAt = time.Now()
	s.Dirty = false
}

// Snapshot returns a copy of active (non-ended) sessions, sorted most-recent-first.
func (r *Registry) Snapshot() []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]*Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		if s.Status == StatusEnded {
			continue
		}
		cp := *s
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LastEvent.After(out[j].LastEvent)
	})
	return out
}
