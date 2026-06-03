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
- **Three auth methods, resolved by `creds.Resolve()`** with Claude Code's own env precedence: `CLAUDE_CODE_USE_BEDROCK` truthy → Bedrock; else `ANTHROPIC_API_KEY` non-empty → x-api-key; else keychain OAuth. The OAuth path reads `Claude Code-credentials` from the macOS keychain on every call (so token refreshes from a parallel `claude` login are picked up automatically) and requires two non-defaults in `summarizer.callMessages`: the `anthropic-beta: oauth-2025-04-20` header AND a `system` prompt that begins with the literal Claude Code identity string (`claudeCodeIdent` constant). Removing either breaks OAuth auth specifically — they're tied to how Anthropic gates OAuth-token inference (the API-key path needs neither, but shares the body builder).
- **Bedrock requests differ in shape, not schema.** `summarizer.callBedrock` puts `anthropic_version: "bedrock-2023-05-31"` in the body and **no `model` field** — the model id is the `InvokeModel` `ModelId` param, defaulting to `eu.anthropic.claude-haiku-4-5-20251001-v1:0` (override via `ANTHROPIC_SMALL_FAST_MODEL`). The response body is the same Messages schema, parsed by the shared `parseMessagesResponse`. The Bedrock client is built lazily via `sync.Once` (`config.LoadDefaultConfig` walks the full AWS credential chain) and a load error is cached for the process lifetime.
- **Status transitions live in `registry.Apply`'s switch on `HookEventName`** (`PreToolUse`/`PostToolUse`/`UserPromptSubmit` → running; `Notification`/`PermissionRequest` → awaiting; `Stop`/`SubagentStop` → idle; `SessionEnd` → ended). New cmux hook names need to be added here or sessions get stuck in the wrong state.
- **`SessionEnd` only fires on a *graceful* Claude quit** (`/exit`, ctrl-d, `clear`). Closing the cmux tab kills the process abruptly, so no hook runs — the only signal is a `workspace.closed` / `surface.closed` event (categories `workspace`/`surface`, **not** `agent`). The subscriber forwards those (see `Event.ClosesWorkspace`), and `ui.Update` routes them to `registry.EndWorkspace`, which ends every session sharing the closed `workspace_id`. Correlation is workspace-level only — agent hook payloads carry `workspace_id` but no pane/surface id, so closing one terminal in a multi-terminal workspace ends all its sessions.
- **Ended sessions are filtered out of `Snapshot`** — they stay in the map but never appear in the UI. Don't expect `len(sessions)` in the registry to match the displayed row count.
- **Cursor file** at `~/.cache/claude-tower/cursor` lets the cmux subscriber resume after restart. The subprocess auto-reconnects with a 2s backoff on exit.

## Fallback behaviour

Auth failures degrade, they don't error. `summarizer.summarize` distinguishes two stages: *resolve-stage* failures (keychain missing, OAuth token expired, AWS `LoadDefaultConfig` error) return a humanized version of the transcript's `slug` field with a nil error, while *call-stage* failures (HTTP non-200, `InvokeModel` errors, response-parse errors) propagate as `Result.Err`. This is intentional — the UI should keep working without auth — so any new auth/setup paths should preserve a non-empty fallback string rather than surfacing the error to the user.
