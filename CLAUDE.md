# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
go mod tidy          # sync deps
go build             # produces ./claude-tower
go vet ./...         # static checks
go test ./...        # run all tests (none yet)
go test ./internal/registry -run TestApply   # single test pattern, once tests exist
./claude-tower      # run the TUI
```

**macOS only** — depends on the `security` CLI to read the keychain and assumes the `cmux` binary is on PATH.

## Architecture

A single-process pipeline that fans events through three goroutines into one Bubble Tea program. Reading `main.go` alone is misleading; the real shape is the data flow:

```
cmux events (subprocess, NDJSON) ──► cmuxevents.Subscribe channel
                                              │
                                              ▼
                                      ui.Update (EventMsg)
                                              │
                              ┌───────────────┴───────────────┐
                              ▼                               ▼
                       registry.Apply              summarizer.Request (debounced 5s)
                       (session state)                        │
                                                              ▼
                                                  transcript.Tail + creds.LoadFromKeychain
                                                              │
                                                              ▼
                                                       Haiku API call
                                                              │
                                                              ▼
                                                  registry.SetSummary (callback)
```

Key invariants when changing this pipeline:

- **The Bubble Tea program is the single owner of UI state.** Anything mutating `registry` from outside the `tea.Program` goroutine must funnel back via `p.Send(...)` or a `SetSummary` callback — never block in `Update`.
- **`summarizer.Request` is debounced per-session (5s).** Rapid event bursts collapse into one Haiku call. Don't add a path that calls `summarize` synchronously from `Update`.
- **Session IDs are normalized by stripping the `claude-` prefix** in `registry.Apply`. The same ID format is used as the transcript filename (`~/.claude/projects/<encoded-cwd>/<sid>.jsonl`), so don't re-add the prefix downstream.
- **`cwd` is encoded for the transcript path by replacing `/` with `-`** (`transcript.encodeCwd`). This matches Claude Code's own directory layout — don't `filepath.Join` raw cwd segments.

## Non-obvious constraints

- **cmux redacts tool inputs and prompt text by design.** The summary path therefore *cannot* rely on event payloads — it tails the JSONL transcript instead. If you need richer signals for the summary, extend `transcript.Tail`, not the event parser.
- **OAuth, not API key.** `creds.LoadFromKeychain` reads `Claude Code-credentials` from the macOS keychain on every call (so token refreshes from a parallel `claude` login are picked up automatically). The Anthropic API call in `summarizer.callHaiku` therefore requires two non-default headers: `anthropic-beta: oauth-2025-04-20` AND a `system` prompt that begins with the literal Claude Code identity string (`claudeCodeIdent` constant). Removing either breaks auth — these are tied to how Anthropic gates OAuth-token inference.
- **Status transitions live in `registry.Apply`'s switch on `HookEventName`** (`PreToolUse`/`PostToolUse`/`UserPromptSubmit` → running; `Notification`/`PermissionRequest` → awaiting; `Stop`/`SubagentStop` → idle; `SessionEnd` → ended). New cmux hook names need to be added here or sessions get stuck in the wrong state.
- **`SessionEnd` only fires on a *graceful* Claude quit** (`/exit`, ctrl-d, `clear`). Closing the cmux tab kills the process abruptly, so no hook runs — the only signal is a `workspace.closed` / `surface.closed` event (categories `workspace`/`surface`, **not** `agent`). The subscriber forwards those (see `Event.ClosesWorkspace`), and `ui.Update` routes them to `registry.EndWorkspace`, which ends every session sharing the closed `workspace_id`. Correlation is workspace-level only — agent hook payloads carry `workspace_id` but no pane/surface id, so closing one terminal in a multi-terminal workspace ends all its sessions.
- **Ended sessions are filtered out of `Snapshot`** — they stay in the map but never appear in the UI. Don't expect `len(sessions)` in the registry to match the displayed row count.
- **Cursor file** at `~/.cache/claude-tower/cursor` lets the cmux subscriber resume after restart. The subprocess auto-reconnects with a 2s backoff on exit.

## Fallback behaviour

If the keychain token is missing or expired, `summarizer.summarize` returns a humanized version of the transcript's `slug` field rather than calling the API. This is intentional — the UI should keep working without auth — so any new error paths should preserve a non-empty fallback string rather than surfacing the error to the user.
