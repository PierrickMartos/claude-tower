package summarizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"claude-tower/internal/creds"
	"claude-tower/internal/transcript"
)

const (
	apiURL          = "https://api.anthropic.com/v1/messages"
	model           = "claude-haiku-4-5-20251001"
	debounce        = 5 * time.Second
	anthropicBeta   = "oauth-2025-04-20"
	claudeCodeIdent = "You are Claude Code, Anthropic's official CLI for Claude."
)

type Result struct {
	SessionID string
	Summary   string
	Err       error
}

type Summarizer struct {
	client *http.Client

	mu      sync.Mutex
	pending map[string]*time.Timer
}

func New() *Summarizer {
	return &Summarizer{
		client:  &http.Client{Timeout: 20 * time.Second},
		pending: map[string]*time.Timer{},
	}
}

// Request schedules a debounced summary for the session. onResult is called
// from a separate goroutine when the LLM responds (or on fallback / error).
func (s *Summarizer) Request(sessionID, cwd string, onResult func(Result)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.pending[sessionID]; ok {
		t.Stop()
	}
	s.pending[sessionID] = time.AfterFunc(debounce, func() {
		s.mu.Lock()
		delete(s.pending, sessionID)
		s.mu.Unlock()

		summary, err := s.summarize(sessionID, cwd)
		onResult(Result{SessionID: sessionID, Summary: summary, Err: err})
	})
}

func (s *Summarizer) summarize(sessionID, cwd string) (string, error) {
	snap, err := transcript.Tail(cwd, sessionID, 20)
	if err != nil {
		return "", fmt.Errorf("tail transcript: %w", err)
	}
	c, err := creds.LoadFromKeychain()
	if err != nil || c.Expired() {
		if snap.Slug != "" {
			return strings.ReplaceAll(snap.Slug, "-", " "), nil
		}
		return "[no summary]", nil
	}
	return s.callHaiku(c, snap)
}

func (s *Summarizer) callHaiku(c *creds.Creds, snap *transcript.Snapshot) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 60,
		"system":     claudeCodeIdent,
		"messages": []map[string]any{
			{"role": "user", "content": buildPrompt(snap)},
		},
	})
	req, err := http.NewRequestWithContext(context.Background(), "POST", apiURL, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", anthropicBeta)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("haiku api %d: %s", resp.StatusCode, string(raw))
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	for _, c := range out.Content {
		if c.Type == "text" {
			return strings.TrimSpace(c.Text), nil
		}
	}
	return "", errors.New("no text in response")
}

func buildPrompt(snap *transcript.Snapshot) string {
	var b strings.Builder
	b.WriteString("Summarise what this Claude Code session is currently doing, in ONE short sentence (max 12 words). Use present-continuous tense. No quotes, no prefix.\n\n")
	if snap.Slug != "" {
		b.WriteString("Conversation slug: ")
		b.WriteString(snap.Slug)
		b.WriteString("\n")
	}
	if t := truncate(snap.LastUserText, 400); t != "" {
		b.WriteString("Latest user prompt: ")
		b.WriteString(t)
		b.WriteString("\n")
	}
	if t := truncate(snap.LastAssistant, 400); t != "" {
		b.WriteString("Latest assistant text: ")
		b.WriteString(t)
		b.WriteString("\n")
	}
	if snap.LastToolUse != "" {
		b.WriteString("Latest tool used: ")
		b.WriteString(snap.LastToolUse)
		b.WriteString("\n")
	}
	return b.String()
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
