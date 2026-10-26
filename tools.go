package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Env is what tools run against.
type Env struct {
	Workdir string
	Cfg     *Config
	Shell   []string // e.g. ["sh","-c"]
}

type Tool struct {
	Name    string
	Desc    string
	Schema  map[string]any
	Kind    string // read | edit | net | exec  (decides approvals)
	Summary func(in json.RawMessage) string
	Run     func(ctx context.Context, e *Env, in json.RawMessage) (string, error)
}

var (
	tools      []*Tool
	toolByName = map[string]*Tool{}
)

func init() {
	for _, t := range []*Tool{toolBash(), toolRead(), toolWrite(), toolEdit(), toolList(), toolGrep(), toolFetch()} {
		registerTool(t)
	}
}

// registerTool is called at startup only (built-ins and MCP), before serving.
func registerTool(t *Tool) {
	if _, dup := toolByName[t.Name]; dup {
		return
	}
	if t.Summary == nil {
		t.Summary = func(in json.RawMessage) string { return trunc(string(in), 1500) }
	}
	tools = append(tools, t)
	toolByName[t.Name] = t
}

// shellArgs builds the argv for one command. PowerShell gets -EncodedCommand so
// quotes, $ and newlines in the model's command survive Windows argv quoting.
func shellArgs(sh []string, command string) []string {
	switch strings.ToLower(strings.TrimSuffix(shellName(sh), ".exe")) {
	case "powershell", "pwsh":
		script := "$ProgressPreference='SilentlyContinue'; [Console]::OutputEncoding=[System.Text.Encoding]::UTF8\n" +
			command + "\nif ($LASTEXITCODE) { exit $LASTEXITCODE }"
		u := utf16.Encode([]rune(script))
		b := make([]byte, len(u)*2)
		for i, c := range u {
			b[2*i], b[2*i+1] = byte(c), byte(c>>8)
		}
		var args []string
		for _, a := range sh[1:] {
			if !strings.EqualFold(a, "-Command") && !strings.EqualFold(a, "-c") {
				args = append(args, a)
			}
		}
		return append(args, "-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	}
	return append(append([]string{}, sh[1:]...), command)
}

func toolDefs() []ToolDef {
	defs := make([]ToolDef, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, ToolDef{Name: t.Name, Description: t.Desc, Schema: t.Schema})
	}
	return defs
}

func toolSummary(name string, in json.RawMessage) string {
	if t, ok := toolByName[name]; ok && t.Summary != nil {
		return t.Summary(in)
	}
	return name
}

// ----------------------------------------------------------------- helpers

func schema(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}
func sType(t, desc string) map[string]any { return map[string]any{"type": t, "description": desc} }

func argStr(in json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(in, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func shellName(sh []string) string {
	if len(sh) == 0 {
		return "sh"
	}
	return filepath.Base(sh[0])
}

// resolve maps a model-supplied path into the workspace and rejects escapes
// (including via symlinks) unless allow_outside_workspace is set.
func (e *Env) resolve(p string) (string, error) {
	if p == "" {
		p = "."
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(e.Workdir, p)
	}
	p = filepath.Clean(p)
	if e.Cfg != nil && e.Cfg.AllowOutside {
		return p, nil
	}
	if !within(evalExisting(e.Workdir), evalExisting(p)) {
		return "", fmt.Errorf("path %q is outside the workspace (%s)", p, e.Workdir)
	}
	return p, nil
}

// evalExisting resolves symlinks for the longest existing prefix of p.
func evalExisting(p string) string {
	cur := p
	var rest []string
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(rest) - 1; i >= 0; i-- {
				r = filepath.Join(r, rest[i])
			}
			return r
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}

func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// outputCap is the per-result budget from max_tool_output_chars.
func outputCap(c *Config) int {
	if c == nil || c.MaxToolOutputChars <= 0 {
		return 16000
	}
	return c.MaxToolOutputChars
}

func omittedNote(lines, n int) string {
	return fmt.Sprintf("\n[... %d lines / %d bytes omitted ...]\n", lines, n)
}

// clipOutput keeps the head and tail of a tool result within max bytes and
// marks what was dropped from the middle. Every tool result goes through it.
func clipOutput(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	half := (max - 64) / 2 // room for the marker
	if half < 0 {
		half = 0
	}
	h := half
	for h > 0 && !utf8.RuneStart(s[h]) {
		h--
	}
	if i := strings.LastIndexByte(s[:h], '\n'); i >= 0 && i >= h*3/4 {
		h = i + 1 // cut on a line break when one is near
	}
	t := len(s) - half
	for t < len(s) && !utf8.RuneStart(s[t]) {
		t++
	}
	if i := strings.IndexByte(s[t:], '\n'); i >= 0 && i < half/4 {
		t += i + 1
	}
	mid := s[h:t]
	return s[:h] + omittedNote(strings.Count(mid, "\n"), len(mid)) + s[t:]
}

// capWriter keeps the head and tail of a stream and drops the middle, so a
// huge command output never sits in memory.
type capWriter struct {
	mu               sync.Mutex
	head, tail       []byte
	headMax, tailMax int
	total            int
	droppedLines     int
}

func newCapWriter(headMax, tailMax int) *capWriter {
	return &capWriter{headMax: headMax, tailMax: tailMax}
}

func (w *capWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := len(p)
	w.total += n
	if len(w.head) < w.headMax {
		k := w.headMax - len(w.head)
		if k > len(p) {
			k = len(p)
		}
		w.head = append(w.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		w.tail = append(w.tail, p...)
		if len(w.tail) > w.tailMax*2 {
			cut := len(w.tail) - w.tailMax
			w.droppedLines += bytes.Count(w.tail[:cut], []byte{'\n'})
			w.tail = append([]byte(nil), w.tail[cut:]...)
		}
	}
	return n, nil
}

func (w *capWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	head, tail, lines := w.head, w.tail, w.droppedLines
	if len(tail) > w.tailMax {
		lines += bytes.Count(tail[:len(tail)-w.tailMax], []byte{'\n'})
		tail = tail[len(tail)-w.tailMax:]
	}
	dropped := w.total - len(head) - len(tail)
	if dropped > 0 {
		// cut on line breaks when one is near, like clipOutput
		if i := bytes.LastIndexByte(head, '\n'); i >= 0 && i >= len(head)*3/4 {
			dropped += len(head) - i - 1
			head = head[:i+1]
		}
		if i := bytes.IndexByte(tail, '\n'); i >= 0 && i < len(tail)/4 {
			dropped, lines = dropped+i+1, lines+1
			tail = tail[i+1:]
		}
	}
	s := string(head)
	if dropped > 0 {
		s += omittedNote(lines, dropped)
	}
	s += string(tail)
	return strings.ToValidUTF8(s, "?")
}

// ---------------------------------------------------------------------- bash

func toolBash() *Tool {
	return &Tool{
		Name: "bash", Kind: "exec",
		Desc: "Run a shell command in the workspace directory and return combined stdout/stderr. " +
			"The user must approve most commands, so prefer read_file/list_dir/grep for reading. " +
			"Long output is truncated in the middle.",
		Schema: schema(map[string]any{
			"command":     sType("string", "The command line to run."),
			"timeout_sec": sType("integer", "Optional timeout in seconds (default from config, max 600)."),
		}, "command"),
		Summary: func(in json.RawMessage) string { return trunc(argStr(in, "command"), 2000) },
		Run:     runBash,
	}
}

func runBash(ctx context.Context, e *Env, in json.RawMessage) (string, error) {
	var a struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout_sec"`
	}
	if err := json.Unmarshal(in, &a); err != nil {
		return "", err
	}
	if strings.TrimSpace(a.Command) == "" {
		return "", errors.New("empty command")
	}
	timeout := time.Duration(e.Cfg.BashTimeoutSec) * time.Second
	if a.Timeout > 0 && a.Timeout <= 600 {
		timeout = time.Duration(a.Timeout) * time.Second
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(tctx, e.Shell[0], shellArgs(e.Shell, a.Command)...)
	cmd.Dir = e.Workdir
	setupProc(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 3 * time.Second
	half := (outputCap(e.Cfg) - 200) / 2 // leave room for the exit-code line
	w := newCapWriter(half, half)
	cmd.Stdout = w
	cmd.Stderr = w

	err := cmd.Run()
	out := w.String()
	var ee *exec.ExitError
	switch {
	case errors.Is(tctx.Err(), context.DeadlineExceeded):
		out += fmt.Sprintf("\n[timed out after %s, process killed]", timeout)
	case ctx.Err() != nil:
		out += "\n[cancelled]"
	case errors.As(err, &ee):
		out += fmt.Sprintf("\n[exit code %d]", ee.ExitCode())
	case err != nil:
		return "", err
	}
	return out, nil
}

// ---------------------------------------------------------------- read_file

func toolRead() *Tool {
	return &Tool{
		Name: "read_file", Kind: "read",
		Desc: "Read a text file inside the workspace. Returns numbered lines. Big files come back one chunk at a time: " +
			"use offset/limit to read a range, or grep to find the lines you need first.",
		Schema: schema(map[string]any{
			"path":   sType("string", "File path, relative to the workspace or absolute inside it."),
			"offset": sType("integer", "1-based line to start from (default 1)."),
			"limit":  sType("integer", "Max lines to return (default and max 2000)."),
		}, "path"),
		Summary: func(in json.RawMessage) string { return argStr(in, "path") },
		Run:     runRead,
	}
}

func runRead(ctx context.Context, e *Env, in json.RawMessage) (string, error) {
	var a struct {
		Path          string `json:"path"`
		Offset, Limit int
	}
	if err := json.Unmarshal(in, &a); err != nil {
		return "", err
	}
	p, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", errors.New("that is a directory; use list_dir")
	}
	if st.Size() > 8<<20 {
		return "", errors.New("file is larger than 8 MB; use grep or bash to inspect it")
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	head := data
	if len(head) > 8000 {
		head = head[:8000]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "", errors.New("binary file")
	}
	if len(data) == 0 {
		return "(empty file)", nil
	}
	lines := strings.Split(string(data), "\n")
	off := a.Offset
	if off < 1 {
		off = 1
	}
	lim := a.Limit
	if lim <= 0 || lim > 2000 {
		lim = 2000
	}
	if off > len(lines) {
		return fmt.Sprintf("(offset beyond end; file has %d lines)", len(lines)), nil
	}
	end := off - 1 + lim
	if end > len(lines) {
		end = len(lines)
	}
	budget := outputCap(e.Cfg) - 300 // leave room for the note
	var b strings.Builder
	last := end
	for i := off - 1; i < end; i++ {
		line := fmt.Sprintf("%6d\t%s\n", i+1, trunc(strings.TrimSuffix(lines[i], "\r"), 2000))
		if b.Len() > 0 && b.Len()+len(line) > budget {
			last = i
			break
		}
		b.WriteString(line)
	}
	if last < len(lines) {
		fmt.Fprintf(&b, "[showing lines %d-%d of %d (%d bytes); continue with offset=%d, or grep for what you need]\n",
			off, last, len(lines), len(data), last+1)
	}
	return b.String(), nil
}

// --------------------------------------------------------------- write_file

func toolWrite() *Tool {
	return &Tool{
		Name: "write_file", Kind: "edit",
		Desc: "Create or overwrite a file inside the workspace (parent directories are created).",
		Schema: schema(map[string]any{
			"path":    sType("string", "File path."),
			"content": sType("string", "Full file content."),
		}, "path", "content"),
		Summary: func(in json.RawMessage) string {
			c := argStr(in, "content")
			return fmt.Sprintf("%s\n(%d bytes)\n%s", argStr(in, "path"), len(c), trunc(c, 600))
		},
		Run: func(ctx context.Context, e *Env, in json.RawMessage) (string, error) {
			var a struct{ Path, Content string }
			if err := json.Unmarshal(in, &a); err != nil {
				return "", err
			}
			p, err := e.resolve(a.Path)
			if err != nil {
				return "", err
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				return "", err
			}
			if err := os.WriteFile(p, []byte(a.Content), 0o644); err != nil {
				return "", err
			}
			return fmt.Sprintf("wrote %d bytes to %s", len(a.Content), a.Path), nil
		},
	}
}

// ---------------------------------------------------------------- edit_file

func toolEdit() *Tool {
	return &Tool{
		Name: "edit_file", Kind: "edit",
		Desc: "Replace text in an existing file. old_string must match exactly once (include enough context), " +
			"unless replace_all is true.",
		Schema: schema(map[string]any{
			"path":        sType("string", "File path."),
			"old_string":  sType("string", "Exact text to replace."),
			"new_string":  sType("string", "Replacement text."),
			"replace_all": sType("boolean", "Replace every occurrence."),
		}, "path", "old_string", "new_string"),
		Summary: func(in json.RawMessage) string {
			return fmt.Sprintf("%s\n- %s\n+ %s", argStr(in, "path"),
				trunc(argStr(in, "old_string"), 500), trunc(argStr(in, "new_string"), 500))
		},
		Run: runEdit,
	}
}

func runEdit(ctx context.Context, e *Env, in json.RawMessage) (string, error) {
	var a struct {
		Path       string `json:"path"`
		Old        string `json:"old_string"`
		New        string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
	}
	if err := json.Unmarshal(in, &a); err != nil {
		return "", err
	}
	if a.Old == "" {
		return "", errors.New("old_string is empty; use write_file to create files")
	}
	p, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	text := string(data)
	old, repl := a.Old, a.New
	// Files with CRLF line endings: let the model write plain \n.
	if strings.Contains(text, "\r\n") && !strings.Contains(old, "\r\n") {
		old = strings.ReplaceAll(old, "\n", "\r\n")
		repl = strings.ReplaceAll(repl, "\n", "\r\n")
	}
	n := strings.Count(text, old)
	switch {
	case n == 0:
		return "", errors.New("old_string not found (check whitespace and indentation)")
	case n > 1 && !a.ReplaceAll:
		return "", fmt.Errorf("old_string matches %d places; add more context or set replace_all", n)
	}
	if a.ReplaceAll {
		text = strings.ReplaceAll(text, old, repl)
	} else {
		text = strings.Replace(text, old, repl, 1)
	}
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		return "", err
	}
	return fmt.Sprintf("edited %s (%d replacement(s))", a.Path, n), nil
}

// ----------------------------------------------------------------- list_dir

func toolList() *Tool {
	return &Tool{
		Name: "list_dir", Kind: "read",
		Desc: "List the entries of a directory inside the workspace (directories end with /).",
		Schema: schema(map[string]any{
			"path": sType("string", "Directory path (default: workspace root)."),
		}),
		Summary: func(in json.RawMessage) string { return argStr(in, "path") },
		Run: func(ctx context.Context, e *Env, in json.RawMessage) (string, error) {
			var a struct{ Path string }
			_ = json.Unmarshal(in, &a)
			p, err := e.resolve(a.Path)
			if err != nil {
				return "", err
			}
			ents, err := os.ReadDir(p)
			if err != nil {
				return "", err
			}
			sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
			var b strings.Builder
			for i, de := range ents {
				if i >= 300 {
					fmt.Fprintf(&b, "[%d more entries]\n", len(ents)-i)
					break
				}
				if de.IsDir() {
					fmt.Fprintf(&b, "%s/\n", de.Name())
				} else if info, err := de.Info(); err == nil {
					fmt.Fprintf(&b, "%s  (%d B)\n", de.Name(), info.Size())
				} else {
					fmt.Fprintf(&b, "%s\n", de.Name())
				}
			}
			if b.Len() == 0 {
				return "(empty directory)", nil
			}
			return b.String(), nil
		},
	}
}

// --------------------------------------------------------------------- grep

func toolGrep() *Tool {
	return &Tool{
		Name: "grep", Kind: "read",
		Desc: "Search file contents with a regular expression (Go/RE2 syntax) under a path in the workspace. " +
			"Skips .git, node_modules, binaries and files over 2 MB. Returns path:line:text.",
		Schema: schema(map[string]any{
			"pattern":     sType("string", "Regular expression."),
			"path":        sType("string", "Directory or file to search (default: workspace root)."),
			"glob":        sType("string", "Optional file name filter, e.g. *.go"),
			"ignore_case": sType("boolean", "Case-insensitive match."),
		}, "pattern"),
		Summary: func(in json.RawMessage) string {
			return argStr(in, "pattern") + "  in  " + argStr(in, "path")
		},
		Run: runGrep,
	}
}

func runGrep(ctx context.Context, e *Env, in json.RawMessage) (string, error) {
	var a struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignore_case"`
	}
	if err := json.Unmarshal(in, &a); err != nil {
		return "", err
	}
	pat := a.Pattern
	if a.IgnoreCase {
		pat = "(?i)" + pat
	}
	re, err := regexp.Compile(pat)
	if err != nil {
		return "", err
	}
	root, err := e.resolve(a.Path)
	if err != nil {
		return "", err
	}
	const maxMatches = 200
	budget := outputCap(e.Cfg) - 200
	var b strings.Builder
	count, full := 0, false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".venv", "__pycache__":
				return filepath.SkipDir
			}
			return nil
		}
		if a.Glob != "" {
			if ok, _ := filepath.Match(a.Glob, d.Name()); !ok {
				return nil
			}
		}
		if info, err := d.Info(); err != nil || info.Size() > 2<<20 || !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer f.Close()
		br := bufio.NewReader(f)
		if peek, _ := br.Peek(4096); bytes.IndexByte(peek, 0) >= 0 {
			return nil
		}
		rel, _ := filepath.Rel(e.Workdir, path)
		sc := bufio.NewScanner(br)
		sc.Buffer(make([]byte, 64*1024), 1<<20)
		for ln := 1; sc.Scan(); ln++ {
			if re.MatchString(sc.Text()) {
				line := fmt.Sprintf("%s:%d:%s\n", rel, ln, trunc(sc.Text(), 300))
				if b.Len()+len(line) > budget {
					full = true
					return fs.SkipAll
				}
				b.WriteString(line)
				count++
				if count >= maxMatches {
					return fs.SkipAll
				}
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return "", walkErr
	}
	if count == 0 {
		return "(no matches)", nil
	}
	if full {
		fmt.Fprintf(&b, "[output limit reached after %d matches; narrow the pattern, path or glob]\n", count)
	} else if count >= maxMatches {
		b.WriteString("[stopped at 200 matches; narrow the search]\n")
	}
	return b.String(), nil
}

// defaultShell picks a shell for the bash tool.
func defaultShell(override []string) []string {
	if len(override) >= 2 {
		return override
	}
	if runtime.GOOS == "windows" {
		for _, name := range []string{"powershell.exe", "pwsh.exe"} {
			if _, err := exec.LookPath(name); err == nil {
				return []string{name, "-NoProfile", "-NonInteractive", "-Command"}
			}
		}
		return []string{"cmd.exe", "/C"}
	}
	return []string{"sh", "-c"}
}
