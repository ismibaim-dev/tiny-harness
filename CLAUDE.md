# tiny-harness — project handover

Lightweight AI coding-agent harness, controlled from a phone. Single Go binary, stdlib only,
serves its own mobile web UI. Owner runs it on his Windows laptop and reaches it via Tailscale.
Current version: **0.3.0** (`const version` in main.go).

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
| jev.go | optional TypeSafe Jev risk check (shadow mode) + `-risk-report` |
| mcp.go | minimal stdio MCP client (initialize, tools/list paginated, tools/call) |
| proc_unix.go / proc_windows.go | kill whole process tree on timeout/stop |
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
- Context: `prepare()` elides old tool output/images/reasoning past `max_context_chars`, then auto-compacts
  (model-written summary replaces history before the current turn). Manual compact via `/api/compact`.
  `repairHistory()` adds results for dangling tool_use blocks.
- Phone sync: `/api/poll` long-poll with `since`, `epoch`, `lv` (live-text version). Streaming text lives in
  `Session.live`; `finishLive()` swaps it for final events atomically.
- Windows shell defaults to PowerShell with `-EncodedCommand` (UTF-16LE base64) to avoid quoting bugs.

## Build / test
```
go vet ./... && go test -race ./...
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o dist/tiny-harness.exe .
```
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

## TypeSafe Jev risk check (owner has early access + $5 credit)
Jev is a "System One" decision model: it answers typed questions about a `state` with probabilities and
scores. It generates no text and calls no tools, so it is NOT a chat provider. It is a small optional
"risk check" on each approval card.

**Status (0.3.0): phase 1 shadow built** in `jev.go`, hooked in `askApproval`. Deviations from the plan below:
- In shadow mode the check runs *beside* the card, not before it, so it adds zero latency; `latency_ms` is
  logged to judge whether the 3 s budget is realistic for badge mode, which must await it before the card.
- Decision values are `allowed` / `denied` / `no_answer` (turn stopped or 30 min timeout), so non-taps
  don't pollute calibration. Log lines also carry `model`, `label`, `reasons`, `latency_ms`, `error`.
- `mode: "badge"` is accepted but runs as shadow until phase 2 exists. `timeout_ms` and `base_url` are configurable.
- `tiny-harness -risk-report` summarises the log (label x decision, denied-but-green, allowed-but-red).
- API shape checked against the live api.typesafe.ai/openapi.json (Oct 25 2026); score levels start at 0.
  NOT verified: a real call with the owner's key.

### API (from docs.typesafe.ai, Oct 2026)
- `POST https://api.typesafe.ai/v1/systemone`, headers `Authorization: Bearer $TYPESAFE_API_KEY`,
  `Content-Type: application/json`. Models: `GET /v1/models`. OpenAPI: https://api.typesafe.ai/openapi.json
- Request: `{"model": "...", "state": <string|object|array>, "questions": {<name>: <question>}}`
  - noul: `{"type":"noul","instructions":"...","criteria":{"true":"...","false":"..."}}` (criteria optional)
  - choice: `{"type":"choice","instructions":"...","criteria":{"label":"description or null", ...}}` (1–255)
  - score: `{"type":"score","instructions":"...","criteria":["lowest", ..., "highest"]}` (1–10, index = score)
- Response: `{"model","answers":{<name>:{...}},"usage":{"input_tokens","output_tokens"}}`
  - noul → `noul` 0..1 (no confidence field); choice → `choice`, `confidence`, `probabilities`;
    score → `score` (weighted, may be fractional), `confidence`, `legend`, `probabilities`.
- All questions run in parallel and independently; adding questions barely changes latency.
- Models: `jev-1.14.0` (newest, = `jev-latest`), `jev-1.13.0`. Pin a version once thresholds are tuned.
- Limits: 64k tokens per request / 32k per question (262k with header `TypeSafe-Danger-Zone-Long-Context: true`,
  slower and less accurate). Body ≤ 32 MiB. English works best. Text only (images need a danger-zone header).
- Errors: 400 rule/context, 401 key, 413 size, 415 encoding, 422 validation (`detail[]`), 429 rate limit,
  529 overloaded. Retry only 429/529 with backoff, honour `retry-after`.
- Price: $0.042 per million input tokens, output free. A check is ~500–900 tokens, so $5 ≈ 100k+ checks.

### Design (agreed direction)
- New file `jev.go` (~150–200 lines, stdlib only). Config `"risk_check": {"model": "jev-1.14.0",
  "api_key_env": "TYPESAFE_API_KEY", "mode": "shadow" | "badge"}`; off when absent or key unset.
- Call it in `askApproval` (and for exec/edit/net/MCP tools only), with a ~3 s timeout. **Fail open to the
  normal approval card**: Jev errors never block or skip anything.
- State sent (JSON): `{"user_request": <latest user text>, "tool": <name>, "action": <tool summary/command>,
  "workspace": <path>, "os": <goos>}`. `user_request` is the owner's own words; `action` is model-written and
  may be attacker-influenced.
- Questions (one request), following TypeSafe's guardrails cookbook style of one narrow question each:
  - `destructive` noul: "Does this action delete, overwrite, or irreversibly change files, data, or git history?"
  - `exfiltrates` noul: "Does this action send data from this computer to a remote server?"
  - `installs` noul: "Does this action download, install, or run software from the internet?"
  - `outside_workspace` noul: "Does this action read or change anything outside the workspace directory?"
  - `secrets` noul: "Does this action read, print, or transmit credentials such as API keys, tokens, or passwords?"
  - `off_task` noul: "Is this action unrelated to what the user asked for?"
  - `severity` score: ["Read-only, no effect", "Minor and easy to undo", "Significant or hard to undo",
    "Severe: data loss, security breach, or money spent"]
- Thresholds (cookbook defaults, recalibrate from shadow data): any noul ≥ 0.70 → red; ≥ 0.35 → amber;
  severity ≥ 2.0 → red.
- Phase 1 **shadow**: no UI change. Append JSONL to `~/.tiny-harness/risk-shadow.jsonl`:
  `{ts, session, tool, action, answers, usage, decision: "allowed"|"denied"}` (decision filled after the tap).
- Phase 2 **badge**: show the label + top reasons on the approval card (new fields on the `approval` Event).
- Never auto-approve on Jev alone. If auto-approval is ever added, require BOTH a strict deterministic
  allowlist match AND all nouls < 0.1, and keep it off by default. Reasons: HiddenLayer reported Jev is more
  prompt-injection-prone than LLMs, the `action` text can be attacker-shaped, and calibration is unproven.
- Privacy: every checked command/path goes to TypeSafe. Mention in README; allow per-workspace opt-out
  (`"risk_check_skip_workspaces": [...]`).
- Tests: fake `httptest` server returning the documented response shape; check fail-open on 500/timeout/422,
  JSONL line written, thresholds mapping, and that a Jev error never changes the approval flow.

## Not included on purpose
Sub-agents, plan mode, checkpoints/undo (use git), hooks, TUI. Add only if they stay small.

## Extras
`extras/preview/` holds the simulated-backend preview (sim.js + build.py that merges it into web/index.html).
It was published as a claude.ai artifact for tapping through the UI without a PC. build.py uses absolute
paths from the original sandbox; adjust before reuse.
