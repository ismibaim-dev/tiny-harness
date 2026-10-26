# tiny-harness — project handover

Lightweight AI coding-agent harness, controlled from a phone. Single Go binary, stdlib only,
serves its own mobile web UI. Owner runs it on his Windows laptop and reaches it via Tailscale.
Current version: **0.3.1** (`const version` in main.go).

## Goals and hard constraints
- Stay lightweight: no third-party Go deps, ~7 MB binary, ~9 MB RSS idle (~1.5 MB private; rest is the mapped binary).
  Measure RSS after changes (`VmRSS`/`RssAnon` in /proc/<pid>/status on Linux).
- Phone-first UX: short replies, approvals as tap cards, works over flaky mobile connections (long-poll).
- Safety: "yolo" approval mode can only be set on the PC (flag/config), never from the phone, and is not
  restored from a saved session. File tools are jailed to the workspace (symlink-aware); `bash` and MCP are not.

## Files
| file | what |
|---|---|
| main.go | config + built-in provider presets, flags, startup, workspaces, MCP start |
| server.go | HTTP routes (`/api/*`), token auth, sessions registry, system prompt, AGENTS.md/CLAUDE.md loading |
| agent.go | session state, agent loop, approvals, streaming "live" text, usage, compaction, persistence |
| provider.go | internal Message/Block format, Provider interface, HTTP retry, SSE reader with stall watchdog |
| prov_anthropic.go | Messages API, streaming, prompt caching (system + last block) |
| prov_openai.go | OpenAI-compatible chat/completions (DeepSeek, GLM, OpenRouter, Groq, Mistral, Ollama…) |
| prov_gemini.go | native Gemini streamGenerateContent, thought signatures, schema sanitizer |
| prov_mock.go | scripted test provider (`"type":"mock"`) |
| tools.go | bash/read_file/write_file/edit_file/list_dir/grep, path jail, PowerShell -EncodedCommand |
| fetch.go | web_fetch tool (crude HTML→text) |
| mcp.go | minimal stdio MCP client (initialize, tools/list paginated, tools/call) |
| proc_unix.go / proc_windows.go | kill whole process tree on timeout/stop |
| install.ps1 | Windows installer behind the README's one-line install link |
| .github/workflows/release.yml | builds and publishes release binaries on a `v*` tag |
| web/index.html | the phone UI (embedded with go:embed), vanilla JS, light/dark tokens |
| *_test.go | unit tests incl. fake SSE servers per provider and a fake MCP server (TestMain helper process) |

## Key design decisions (don't regress these)
- Internal `Block` types: text, image, tool_use, tool_result, reasoning. `Sig`/`Src` hold provider-opaque state
  that is echoed back only to the same profile/model.
- **Gemini 3**: echo `thoughtSignature` exactly; for function calls from another model use dummy
  `skip_thought_signature_validator` on the first call part. Tool schemas go through `geminiSchema()` whitelist.
- **DeepSeek thinking mode**: `reasoning_content` must be sent back for all previous assistant turns when tools
  are present (else HTTP 400). Only echoed to the profile that produced it.
- OpenAI-compat: sends `stream_options.include_usage`; auto-retries without it if a provider 400s on it.
- Tool approval by `Tool.Kind`: read (never asks) / edit / net / exec. ask mode asks for all non-read;
  auto-edit asks only for exec (bash, MCP). `safe_commands` = exact prefix match with no shell metacharacters.
  "Allow all this turn" sets `turnAllow` until the turn ends.
- Context: `prepare()` elides old tool output/images/reasoning past `max_context_chars` (default 200000), then
  auto-compacts (model-written summary replaces history before the current turn). Manual compact via
  `/api/compact`. Both use `compact_profile` when set and usable (`Server.compactWith`), else the chat's model.
  `repairHistory()` adds results for dangling tool_use blocks.
- Tool output: every result passes `clipOutput()` in `execTool` (`max_tool_output_chars`, default 16000):
  head + tail, `[... N lines / N bytes omitted ...]`. bash streams into `capWriter` (same marker, bounded RAM);
  read_file, grep and web_fetch page to the same cap with a "continue with offset" note.
- Stable prompt prefix (prompt caching): `systemPrompt()` takes the chat's `started` date (set on its first turn,
  persisted) instead of the clock; MCP servers and their tools register in sorted order. Don't add per-turn
  content to the system prompt or tool list.
- Phone sync: `/api/poll` long-poll with `since`, `epoch`, `lv` (live-text version). Streaming text lives in
  `Session.live`; `finishLive()` swaps it for final events atomically.
- Windows shell defaults to PowerShell with `-EncodedCommand` (UTF-16LE base64) to avoid quoting bugs.

## Build / test
```
go vet ./... && go test -race ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/tiny-harness.exe .
```
Release: bump `const version`, merge to main, push tag `v<version>`. `.github/workflows/release.yml` checks the
tag matches `const version`, runs vet + race tests, builds windows/linux/darwin × amd64/arm64 with the command
above, and publishes them with `SHA256SUMS`. `install.ps1` (README's `irm … | iex` link) installs the latest
release to `%LOCALAPPDATA%\Programs\tiny-harness`, verifies the checksum and adds it to the user PATH via the
registry (keeps `%VARS%` unexpanded). Tested with pwsh 7 on Linux against a local fake release, not on real Windows.
Manual/E2E: run with a mock config, e.g.
```json
{"listen":"127.0.0.1:8798","workdir":"./ws","default_profile":"mock","token":"testtoken-0123456789abcdef",
 "max_context_chars":3000,"profiles":{"mock":{"type":"mock","model":"mock-1"}}}
```
Mock commands: `run <cmd>`, `list`, `write`, `tool <name> <json>`, `slow`. Previously driven with Playwright
at iPhone size (chat, streaming, approve/deny, images, model switch, compaction, stop, reload, workspaces).

## Verified vs not
- Verified: unit tests (race clean), wire formats against recorded-shape SSE streams, fake MCP server,
  headless-Chromium UI run.
- NOT verified: real API calls to any provider, the exe on real Windows, real MCP servers, real phones.
  First real runs should be: Gemini (thought signatures), DeepSeek (reasoning echo), PowerShell commands.
- Model ids in presets come from provider docs as of Oct 2026 and go stale; the UI can load live model lists.

## Not included on purpose
Sub-agents, plan mode, checkpoints/undo (use git), hooks, TUI. Add only if they stay small.
TypeSafe Jev risk check (decided against: no token savings, adds a third-party call and privacy cost; owner approves every action from the phone).

## Extras
`extras/preview/` holds the simulated-backend preview (sim.js + build.py that merges it into web/index.html).
It was published as a claude.ai artifact for tapping through the UI without a PC. build.py uses absolute
paths from the original sandbox; adjust before reuse.
