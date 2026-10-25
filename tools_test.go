package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf16"
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
