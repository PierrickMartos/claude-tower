package transcript

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Discovery struct {
	SessionID string
	Cwd       string
	LastTool  string
	LastEvent time.Time
}

// DiscoverActive returns one Discovery per currently-running claude process,
// derived by parsing `--session-id <uuid>` / `--resume <uuid>` out of `ps`.
// For each session id, locates the matching JSONL transcript and extracts cwd
// and last tool used. Sessions whose transcript is missing or has no cwd are
// skipped.
func DiscoverActive() ([]Discovery, error) {
	ids, err := activeSessionIDs()
	if err != nil {
		return nil, err
	}
	paths, err := indexSessionFiles()
	if err != nil {
		return nil, err
	}
	out := make([]Discovery, 0, len(ids))
	for _, sid := range ids {
		path, ok := paths[sid]
		if !ok {
			continue
		}
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		d := Discovery{SessionID: sid, LastEvent: info.ModTime()}
		scanMeta(path, &d)
		if d.Cwd == "" {
			continue
		}
		out = append(out, d)
	}
	return out, nil
}

var sessionArgRE = regexp.MustCompile(`--(?:session-id|resume)[= ]([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})`)

func activeSessionIDs() ([]string, error) {
	out, err := exec.Command("ps", "axww", "-o", "command").Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, m := range sessionArgRE.FindAllStringSubmatch(string(out), -1) {
		if _, dup := seen[m[1]]; dup {
			continue
		}
		seen[m[1]] = struct{}{}
		ids = append(ids, m[1])
	}
	return ids, nil
}

func indexSessionFiles() (map[string]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	root := filepath.Join(home, ".claude", "projects")
	projects, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		dir := filepath.Join(root, p.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			sid := strings.TrimSuffix(f.Name(), ".jsonl")
			// Prefer the most-recently-modified file when a session id appears
			// in multiple project dirs (e.g. session resumed from a different cwd).
			existing, ok := out[sid]
			if ok {
				ai, _ := f.Info()
				bi, _ := os.Stat(existing)
				if ai != nil && bi != nil && ai.ModTime().Before(bi.ModTime()) {
					continue
				}
			}
			out[sid] = filepath.Join(dir, f.Name())
		}
	}
	return out, nil
}

func scanMeta(path string, d *Discovery) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		var m struct {
			Type    string          `json:"type"`
			Cwd     string          `json:"cwd"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &m); err != nil {
			continue
		}
		if m.Cwd != "" {
			d.Cwd = m.Cwd
		}
		if m.Type == "assistant" {
			if _, tool := extractAssistant(m.Message); tool != "" {
				d.LastTool = tool
			}
		}
	}
}

type Entry struct {
	Type    string          `json:"type"`
	Slug    string          `json:"slug"`
	Message json.RawMessage `json:"message"`
}

type Snapshot struct {
	Slug          string
	LastUserText  string
	LastAssistant string
	LastToolUse   string
	Entries       []Entry
}

func encodeCwd(cwd string) string {
	return strings.ReplaceAll(cwd, "/", "-")
}

func Path(cwd, sessionID string) string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "projects", encodeCwd(cwd), sessionID+".jsonl")
}

// Tail reads the last `n` user/assistant/last-prompt/ai-title entries from a
// Claude Code session transcript and extracts preview fields.
func Tail(cwd, sessionID string, n int) (*Snapshot, error) {
	f, err := os.Open(Path(cwd, sessionID))
	if err != nil {
		return nil, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	ring := make([]Entry, 0, n)
	var slug string

	for scanner.Scan() {
		var e Entry
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			continue
		}
		if e.Slug != "" {
			slug = e.Slug
		}
		switch e.Type {
		case "user", "assistant", "last-prompt", "ai-title":
			if len(ring) >= n {
				ring = ring[1:]
			}
			ring = append(ring, e)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	snap := &Snapshot{Slug: slug, Entries: ring}
	for _, e := range ring {
		switch e.Type {
		case "user":
			if t := extractUserText(e.Message); t != "" {
				snap.LastUserText = t
			}
		case "assistant":
			text, tool := extractAssistant(e.Message)
			if text != "" {
				snap.LastAssistant = text
			}
			if tool != "" {
				snap.LastToolUse = tool
			}
		}
	}
	return snap, nil
}

func extractUserText(raw json.RawMessage) string {
	var m struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	var s string
	if err := json.Unmarshal(m.Content, &s); err == nil {
		return s
	}
	return ""
}

func extractAssistant(raw json.RawMessage) (text, tool string) {
	var m struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Name string `json:"name"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", ""
	}
	for _, c := range m.Content {
		if c.Type == "text" && c.Text != "" {
			text = c.Text
		}
		if c.Type == "tool_use" && c.Name != "" {
			tool = c.Name
		}
	}
	return
}
