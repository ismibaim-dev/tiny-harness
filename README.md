# tiny-harness v0.3.1

A small coding agent you can drive from your phone. One Go binary (~7 MB), no runtime, no dependencies.
Measured on Linux: ~9 MB resident at idle, of which ~1.5 MB is private memory (the rest is the binary itself),
~10 MB after a working session. MCP servers you add run as separate processes and cost whatever they cost.

## What's in it

- **Models:** Anthropic (with prompt caching), **Gemini (native API)**, and anything OpenAI-compatible:
  DeepSeek, GLM/Z.ai, OpenAI, OpenRouter, Groq, Mistral, Kimi, Qwen, xAI, Ollama, LM Studio...
  Switch provider and model per chat from the phone, or load the live model list from the provider.
- **Streaming** replies on the phone.
- **Tools:** `bash`, `read_file`, `write_file`, `edit_file`, `list_dir`, `grep`, `web_fetch`, plus any **MCP** server tools.
- **Images:** attach photos/screenshots from the phone (resized on the phone before upload).
- **Context management:** every tool result is capped (head + tail kept, middle marked as omitted); old tool
  output is trimmed first, then the history is **auto-compacted** into a summary when it passes the budget,
  optionally by a cheaper model (`compact_profile`); there is also a manual "Compact context" button.
- **Prompt caching friendly:** the system prompt and tool list stay byte-identical for the whole chat, so
  Anthropic prompt caching and DeepSeek/Gemini prefix caching keep hitting.
- **Project instructions:** `AGENTS.md` (or `CLAUDE.md`) in the workspace root is added to the system prompt.
- **Workspaces:** several project folders; pick one per chat.
- **Token usage** per chat, approvals with "Allow all this turn", optional push notifications via ntfy.

Not included (on purpose, to stay small): sub-agents, plan mode, checkpoints/undo (use git), hooks, a TUI.

## Quick start (Windows, PowerShell)

```powershell
# set whichever keys you have (setx makes them permanent for new terminals)
$env:GEMINI_API_KEY    = "..."
$env:ANTHROPIC_API_KEY = "..."
$env:DEEPSEEK_API_KEY  = "..."

.\tiny-harness.exe -workdir C:\Users\you\some-project
```

The first run prints an access token (also saved in `%USERPROFILE%\.tiny-harness\token`).
Open `http://127.0.0.1:8787/` on the PC, paste the token, chat. Only profiles whose key is set show up.

### Built-in profiles

| profile    | type      | key env var          | default model |
|------------|-----------|----------------------|---------------|
| claude     | anthropic | `ANTHROPIC_API_KEY`  | claude-sonnet-5-5 |
| gemini     | gemini    | `GEMINI_API_KEY`     | gemini-3.8-flash |
| deepseek   | openai    | `DEEPSEEK_API_KEY`   | deepseek-flash |
| glm        | openai    | `ZAI_API_KEY`        | glm-4.6 |
| openai     | openai    | `OPENAI_API_KEY`     | gpt-5 |
| openrouter | openai    | `OPENROUTER_API_KEY` | openrouter/auto |
| groq       | openai    | `GROQ_API_KEY`       | openai/gpt-oss-120b |
| mistral    | openai    | `MISTRAL_API_KEY`    | mistral-large-latest |

Model names go stale fast. If one fails, open the model chip on the phone → **Load all models from the
provider** and pick a current one, or type any model id.

## Config

`tiny-harness -init` writes `~/.tiny-harness/config.json`. Profiles you define are added to the built-ins
(same name = replaced). Example:

```json
{
  "default_profile": "gemini",
  "workdir": "C:\\Users\\you\\fyp",
  "workspaces": { "web": "C:\\Users\\you\\projects\\site", "plc": "D:\\MEK629" },
  "profiles": {
    "kimi":   { "type": "openai", "model": "kimi-k2-turbo-preview", "base_url": "https://api.moonshot.ai/v1", "api_key_env": "MOONSHOT_API_KEY" },
    "qwen":   { "type": "openai", "model": "qwen-plus", "base_url": "https://dashscope-intl.aliyuncs.com/compatible-mode/v1", "api_key_env": "DASHSCOPE_API_KEY" },
    "local":  { "type": "openai", "model": "qwen3:8b", "base_url": "http://localhost:11434/v1" },
    "gemini-compat": { "type": "openai", "model": "gemini-3.8-flash", "base_url": "https://generativelanguage.googleapis.com/v1beta/openai", "api_key_env": "GEMINI_API_KEY" },
    "deepseek-fast": { "type": "openai", "model": "deepseek-flash", "base_url": "https://api.deepseek.com", "api_key_env": "DEEPSEEK_API_KEY",
                       "extra": { "thinking": { "type": "disabled" } } }
  },
  "mcp_servers": {
    "fs-memory": { "command": "npx", "args": ["-y", "@modelcontextprotocol/server-memory"], "auto_approve": ["*"] }
  },
  "compact_profile": "deepseek-fast",
  "ntfy_url": "https://ntfy.sh/your-secret-topic-name"
}
```

- `extra` is merged into every request body (provider-specific switches: thinking, reasoning effort...).
- `max_tokens` per profile; Gemini defaults to 32768 because thinking shares that budget.
- `max_context_chars` (default 200000 ≈ 50k tokens) sets when auto-compaction kicks in.
- `max_tool_output_chars` (default 16000, min 2000) caps each tool result. Longer output keeps its head and tail
  with `[... N lines / N bytes omitted ...]` in between; `read_file`, `grep` and `web_fetch` stop at the cap
  and say how to read further (offset/limit, a narrower pattern).
- `compact_profile`: profile used to write compaction summaries, e.g. a cheap fast model. If it is unset or
  has no API key, each chat compacts with its own model.
- `shell`: default is PowerShell on Windows. For Git Bash: `"shell": ["C:\\Program Files\\Git\\bin\\bash.exe", "-c"]`.
- `system_extra`: text appended to the system prompt.

## Reaching it from your phone

Do **not** expose this to the internet: it is a remote shell on your machine.

Use Tailscale on both devices, keep the harness on loopback, and let Tailscale proxy it with HTTPS:

```powershell
tailscale serve --bg 8787        # check `tailscale serve --help` if your version differs
```

Open the `https://<pc-name>.<tailnet>.ts.net` URL on the phone and "Add to Home Screen".

## Safety model

- **ask** (default): every `bash`, file write/edit, `web_fetch` and MCP call needs a tap on the phone.
  **auto-edit**: file edits and `web_fetch` run without asking; shell and MCP still ask.
  **yolo**: nothing asks; only settable on the PC (`-approval yolo` or config), never from the phone,
  and it is not restored from a saved chat after a restart.
- "Allow all this turn" lasts until the agent finishes the current request.
- `safe_commands` auto-run only on an exact prefix match with no `; & | < > $` backtick or newline. Convenience, not a sandbox.
- File tools are confined to the chat's workspace (symlinks included). `bash` and MCP servers are **not** confined.
- `web_fetch` can reach anything your PC can (including LAN). The model is told to treat fetched text as data,
  but prompt injection is a real risk: that's what the approval step is for.
- MCP `auto_approve` lists tools that run without asking; only list read-only tools you trust.
- Images and chat history are stored unencrypted in `~/.tiny-harness/sessions/`.

## Build from source

```
go build -trimpath -ldflags="-s -w" -o tiny-harness .      # Go 1.22+
go test ./...
```

## Testing status

Tested: unit tests (incl. race detector) with recorded-format SSE streams for Anthropic, OpenAI-compatible
and Gemini (thought signatures, DeepSeek `reasoning_content` echo, schema sanitizing), a fake MCP server, and a
headless-Chromium run of the phone UI at iPhone size against a mock model (chat, streaming, approve/deny,
allow-all, image upload, model switching, auto/manual compaction, stop, reload, workspaces).

**Not tested:** real API calls to any provider, the Windows binary on real Windows (it cross-compiles and vets
cleanly), real MCP servers, and real phones (Safari/Chrome on device). Gemini's and DeepSeek's wire details
were taken from their docs as of Oct 2026.
