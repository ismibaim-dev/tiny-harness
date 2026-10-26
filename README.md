# tiny-harness v0.3.1

A small coding agent you can drive from your phone. One Go binary (~7 MB), no runtime, no dependencies.
Measured on Linux: ~9 MB resident at idle, of which ~1.5 MB is private memory (the rest is the binary itself),
~10 MB after a working session. MCP servers you add run as separate processes and cost whatever they cost.

**Install on Windows** (PowerShell, no admin needed):

```powershell
irm https://raw.githubusercontent.com/ismibaim-dev/tiny-harness/main/install.ps1 | iex
```

Or download [tiny-harness-windows-amd64.exe](https://github.com/ismibaim-dev/tiny-harness/releases/latest/download/tiny-harness-windows-amd64.exe) yourself.
Full setup, including phone access: [Install (Windows, step by step)](#install-windows-step-by-step).

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

## Install (Windows, step by step)

You need a Windows PC, an API key for at least one model provider ([see the table](#built-in-profiles)),
and, for phone access, Tailscale on the PC and the phone.

**1. Install tiny-harness.** Open PowerShell (Start menu, type "PowerShell"; no admin needed) and paste:

```powershell
irm https://raw.githubusercontent.com/ismibaim-dev/tiny-harness/main/install.ps1 | iex
```

It downloads the latest release to `%LOCALAPPDATA%\Programs\tiny-harness`, checks its SHA-256 checksum,
and adds that folder to your user PATH. To do it by hand instead, download
[tiny-harness-windows-amd64.exe](https://github.com/ismibaim-dev/tiny-harness/releases/latest/download/tiny-harness-windows-amd64.exe)
([arm64](https://github.com/ismibaim-dev/tiny-harness/releases/latest/download/tiny-harness-windows-arm64.exe) for ARM laptops) from the
[latest release](https://github.com/ismibaim-dev/tiny-harness/releases/latest), rename it to `tiny-harness.exe`, and put it in any folder.

**2. Save an API key.** Pick at least one provider from the table below. `setx` keeps the key for new windows:

```powershell
setx GEMINI_API_KEY "paste-your-key-here"
```

**3. Open a new PowerShell window** so it picks up the PATH and the key, and check the install:

```powershell
tiny-harness -version
```

**4. Start it in a project folder:**

```powershell
tiny-harness -workdir C:\Users\you\some-project
```

The first run prints an access token (also saved in `%USERPROFILE%\.tiny-harness\token`).
Keep this window open while you use it.

**5. Try it on the PC.** Open <http://127.0.0.1:8787/>, paste the token, and send a message.
Only profiles whose key is set appear in the model picker.

**6. Reach it from your phone.** Install [Tailscale](https://tailscale.com/download) on the PC and the phone
and sign in to the same account on both. Then, in a second PowerShell window on the PC:

```powershell
tailscale serve --bg 8787
```

It prints an `https://<pc-name>.<tailnet>.ts.net` address. Open it on the phone, paste the token, and use
"Add to Home Screen". Never expose the port to the internet: it is a remote shell on your PC
(see [Reaching it from your phone](#reaching-it-from-your-phone) and [Safety model](#safety-model)).

**Update:** run the step 1 command again. It works while tiny-harness is running; restart it to use the new
version. To install a specific version, run `$env:TINY_HARNESS_VERSION = "v0.3.1"` first.

**Uninstall:** delete `%LOCALAPPDATA%\Programs\tiny-harness` and remove it from your user PATH (Start menu,
"Edit environment variables for your account"). Chats, config and the token are in `%USERPROFILE%\.tiny-harness`;
delete that folder too if you want them gone.

**If Windows warns about the exe:** it is not code-signed, so SmartScreen or Defender may warn about a
manual download. Each release has a `SHA256SUMS` file to check it against; the installer does that for you.

### Linux and macOS

Download the binary for your system (`linux-amd64`, `linux-arm64`, `darwin-arm64` for Apple silicon,
`darwin-amd64`) from the [latest release](https://github.com/ismibaim-dev/tiny-harness/releases/latest):

```sh
curl -fsSL -o tiny-harness https://github.com/ismibaim-dev/tiny-harness/releases/latest/download/tiny-harness-linux-amd64
chmod +x tiny-harness
./tiny-harness -workdir ~/some-project
```

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

Releases are built by `.github/workflows/release.yml`: bump `const version` in main.go, merge, then either push a
matching tag (`git tag v0.3.1 && git push origin v0.3.1`) or, with no PC at hand, open the repo on github.com in
a browser (on a phone too) → **Actions** → **release** → **Run workflow** on `main`, which creates the tag for you.
The workflow runs the tests, cross-compiles Windows/Linux/macOS binaries, and publishes them with `SHA256SUMS`,
which is where the install links point. To build it yourself:

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
