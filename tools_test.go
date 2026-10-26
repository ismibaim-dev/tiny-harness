package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"
)

func testEnv(t *testing.T) *Env {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	return &Env{Workdir: dir, Cfg: &cfg, Shell: defaultShell(nil)}
}

func call(t *testing.T, e *Env, name string, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	return toolByName[name].Run(context.Background(), e, raw)
}

func TestResolveJail(t *testing.T) {
	e := testEnv(t)
	if _, err := e.resolve("a/b.txt"); err != nil {
		t.Fatalf("inside path rejected: %v", err)
	}
	for _, bad := range []string{"../x", "a/../../x", filepath.Dir(e.Workdir)} {
		if _, err := e.resolve(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	if runtime.GOOS != "windows" {
		outside := t.TempDir()
		if err := os.Symlink(outside, filepath.Join(e.Workdir, "link")); err != nil {
			t.Fatal(err)
		}
		if _, err := e.resolve("link/secret.txt"); err == nil {
			t.Error("symlink escape should be rejected")
		}
	}
}

func TestEditFile(t *testing.T) {
	e := testEnv(t)
	if _, err := call(t, e, "write_file", map[string]any{"path": "f.txt", "content": "one\ntwo\ntwo\n"}); err != nil {
		t.Fatal(err)
	}
	if _, err := call(t, e, "edit_file", map[string]any{"path": "f.txt", "old_string": "two", "new_string": "2"}); err == nil {
		t.Error("ambiguous edit should fail")
	}
	if _, err := call(t, e, "edit_file", map[string]any{"path": "f.txt", "old_string": "one", "new_string": "1"}); err != nil {
		t.Fatal(err)
	}
	out, _ := call(t, e, "read_file", map[string]any{"path": "f.txt"})
	if !strings.Contains(out, "1\n") || !strings.Contains(out, "two") {
		t.Errorf("unexpected content: %q", out)
	}
	// CRLF file edited with plain \n in the model's strings
	os.WriteFile(filepath.Join(e.Workdir, "w.txt"), []byte("a\r\nb\r\n"), 0o644)
	if _, err := call(t, e, "edit_file", map[string]any{"path": "w.txt", "old_string": "a\nb", "new_string": "x\ny"}); err != nil {
		t.Errorf("CRLF edit failed: %v", err)
	}
}

func TestGrepAndList(t *testing.T) {
	e := testEnv(t)
	os.WriteFile(filepath.Join(e.Workdir, "a.go"), []byte("package a\nfunc Hello() {}\n"), 0o644)
	os.WriteFile(filepath.Join(e.Workdir, "b.bin"), []byte{0, 1, 2, 'H', 'e', 'l', 'l', 'o'}, 0o644)
	out, err := call(t, e, "grep", map[string]any{"pattern": "hello", "ignore_case": true})
	if err != nil || !strings.Contains(out, "a.go:2:") || strings.Contains(out, "b.bin") {
		t.Errorf("grep: %q %v", out, err)
	}
	out, _ = call(t, e, "list_dir", map[string]any{})
	if !strings.Contains(out, "a.go") {
		t.Errorf("list_dir: %q", out)
	}
}

func TestBashTimeoutAndCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-specific")
	}
	e := testEnv(t)
	out, _ := call(t, e, "bash", map[string]any{"command": "echo hi; exit 3"})
	if !strings.Contains(out, "hi") || !strings.Contains(out, "exit code 3") {
		t.Errorf("exit code: %q", out)
	}
	out, _ = call(t, e, "bash", map[string]any{"command": "sleep 5 & sleep 5", "timeout_sec": 1})
	if !strings.Contains(out, "timed out") {
		t.Errorf("timeout: %q", out)
	}
	out, _ = call(t, e, "bash", map[string]any{"command": "yes | head -c 200000"})
	if len(out) > 26000 || !strings.Contains(out, "bytes omitted") {
		t.Errorf("cap: len=%d", len(out))
	}
}

func TestSafeCommands(t *testing.T) {
	cfg := defaultConfig()
	sv := &Server{cfg: &cfg}
	for cmd, want := range map[string]bool{
		"git status":           true,
		"git status -s":        true,
		"git statusx":          false,
		"git status; rm -rf /": false,
		"git log | sh":         false,
		"date $(whoami)":       false,
		"pwd\nrm x":            false,
		"rm -rf x":             false,
		"git diff > out.patch": false,
	} {
		if got := sv.isSafeCommand(cmd); got != want {
			t.Errorf("isSafeCommand(%q)=%v want %v", cmd, got, want)
		}
	}
}

func TestPowerShellArgs(t *testing.T) {
	args := shellArgs([]string{"powershell.exe", "-NoProfile", "-NonInteractive", "-Command"}, `echo "a $b"`)
	if len(args) != 4 || args[2] != "-EncodedCommand" {
		t.Fatalf("args: %v", args)
	}
	raw, err := base64.StdEncoding.DecodeString(args[3])
	if err != nil || len(raw)%2 != 0 {
		t.Fatal("not UTF-16LE base64")
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	if s := string(utf16.Decode(u)); !strings.Contains(s, `echo "a $b"`) {
		t.Errorf("decoded script: %q", s)
	}
	if got := shellArgs([]string{"sh", "-c"}, "ls"); len(got) != 2 || got[1] != "ls" {
		t.Errorf("sh args: %v", got)
	}
}

func TestHTMLToText(t *testing.T) {
	got := htmlToText(`<html><head><title>x</title></head><body><script>bad()</script><h1>Hi &amp; bye</h1><p>one</p><p>two</p></body></html>`)
	if strings.Contains(got, "bad()") || !strings.Contains(got, "Hi & bye") || !strings.Contains(got, "two") {
		t.Errorf("htmlToText: %q", got)
	}
}

func TestClipOutput(t *testing.T) {
	if got := clipOutput("short", 100); got != "short" {
		t.Errorf("short input changed: %q", got)
	}
	var sb strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&sb, "line %d héllo\n", i)
	}
	in := sb.String()
	out := clipOutput(in, 4000)
	if len(out) > 4000 || !utf8.ValidString(out) {
		t.Fatalf("len=%d valid=%v", len(out), utf8.ValidString(out))
	}
	if !strings.HasPrefix(out, "line 1 héllo\n") || !strings.HasSuffix(out, "line 5000 héllo\n") {
		t.Error("head or tail lost")
	}
	var lines, n int
	if _, err := fmt.Sscanf(out[strings.Index(out, "[... "):], "[... %d lines / %d bytes omitted ...]", &lines, &n); err != nil {
		t.Fatalf("marker: %v\n%s", err, out)
	}
	head, tail, _ := strings.Cut(out, omittedNote(lines, n))
	if len(head)+n+len(tail) != len(in) || strings.Count(in[len(head):len(in)-len(tail)], "\n") != lines {
		t.Errorf("counts: head=%d omitted=%d tail=%d lines=%d of %d", len(head), n, len(tail), lines, len(in))
	}
	if !strings.HasSuffix(head, "llo\n") || !strings.HasPrefix(tail, "line ") {
		t.Error("should cut on line breaks")
	}
	one := strings.Repeat("é", 10000) // no newlines, multibyte
	if got := clipOutput(one, 3000); len(got) > 3000 || !utf8.ValidString(got) || !strings.Contains(got, "[... 0 lines /") {
		t.Errorf("single line: len=%d valid=%v", len(got), utf8.ValidString(got))
	}
}

func TestCapWriterMarker(t *testing.T) {
	w := newCapWriter(1000, 1000)
	var all strings.Builder
	for i := 0; i < 3000; i++ {
		s := fmt.Sprintf("row %d\n", i)
		all.WriteString(s)
		w.Write([]byte(s))
	}
	out, in := w.String(), all.String()
	var lines, n int
	if _, err := fmt.Sscanf(out[strings.Index(out, "[... "):], "[... %d lines / %d bytes omitted ...]", &lines, &n); err != nil {
		t.Fatalf("marker: %v", err)
	}
	head, tail, _ := strings.Cut(out, omittedNote(lines, n))
	if len(head)+n+len(tail) != len(in) || strings.Count(in[len(head):len(in)-len(tail)], "\n") != lines {
		t.Errorf("counts: head=%d omitted=%d tail=%d lines=%d", len(head), n, len(tail), lines)
	}
	if !strings.HasPrefix(in, head) || !strings.HasSuffix(in, tail) || !strings.HasSuffix(head, "\n") || !strings.HasPrefix(tail, "row ") {
		t.Errorf("should keep whole lines: head ends %q, tail starts %q", head[len(head)-8:], tail[:8])
	}
}

func TestBashOutputCapped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses seq")
	}
	e := testEnv(t)
	e.Cfg.MaxToolOutputChars = 4000
	out, err := call(t, e, "bash", map[string]any{"command": "seq 1 100000"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 4000 || !strings.HasPrefix(out, "1\n2\n") || !strings.HasSuffix(strings.TrimSpace(out), "100000") || !strings.Contains(out, " lines / ") {
		t.Errorf("len=%d head=%q tail=%q", len(out), out[:20], out[len(out)-20:])
	}
}

func TestReadFileChunks(t *testing.T) {
	e := testEnv(t)
	e.Cfg.MaxToolOutputChars = 4000
	var sb strings.Builder
	for i := 1; i <= 5000; i++ {
		fmt.Fprintf(&sb, "row %d\n", i)
	}
	if err := os.WriteFile(filepath.Join(e.Workdir, "big.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	out, _ := call(t, e, "read_file", map[string]any{"path": "big.txt"})
	var from, to, total, next int
	note := out[strings.LastIndex(out, "[showing"):]
	if _, err := fmt.Sscanf(note, "[showing lines %d-%d of %d", &from, &to, &total); err != nil || from != 1 || total != 5001 {
		t.Fatalf("note %q: %v", note, err)
	}
	if len(out) > 4000 || !strings.Contains(note, fmt.Sprintf("offset=%d", to+1)) {
		t.Errorf("len=%d note=%q", len(out), note)
	}
	_, _ = fmt.Sscanf(note[strings.Index(note, "offset="):], "offset=%d", &next)
	out2, _ := call(t, e, "read_file", map[string]any{"path": "big.txt", "offset": next})
	if !strings.HasPrefix(strings.TrimSpace(out2), fmt.Sprintf("%d\trow %d", next, next)) {
		t.Errorf("continuation starts with %q", out2[:30])
	}
	out3, _ := call(t, e, "read_file", map[string]any{"path": "big.txt", "offset": 10, "limit": 5})
	if strings.Count(out3, "\trow ") != 5 || !strings.Contains(out3, "[showing lines 10-14 of 5001") {
		t.Errorf("range read: %q", out3)
	}
	_ = os.WriteFile(filepath.Join(e.Workdir, "small.txt"), []byte("a\nb\n"), 0o644)
	if out4, _ := call(t, e, "read_file", map[string]any{"path": "small.txt"}); strings.Contains(out4, "[showing") {
		t.Errorf("small file should have no note: %q", out4)
	}
}

func TestGrepBudget(t *testing.T) {
	e := testEnv(t)
	e.Cfg.MaxToolOutputChars = 2000
	_ = os.WriteFile(filepath.Join(e.Workdir, "m.txt"), []byte(strings.Repeat("needle here and some more text\n", 150)), 0o644)
	out, err := call(t, e, "grep", map[string]any{"pattern": "needle"})
	if err != nil || len(out) > 2000 || !strings.Contains(out, "[output limit reached after") {
		t.Errorf("len=%d err=%v tail=%q", len(out), err, out[max(0, len(out)-80):])
	}
}

func TestWebFetchPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("0123456789", 5000)))
	}))
	defer srv.Close()
	e := testEnv(t)
	e.Cfg.MaxToolOutputChars = 4000
	out, err := call(t, e, "web_fetch", map[string]any{"url": srv.URL})
	if err != nil || len(out) > 4000 || !strings.Contains(out, "[truncated; continue with offset=3700]") {
		t.Errorf("len=%d err=%v tail=%q", len(out), err, out[max(0, len(out)-60):])
	}
}
