package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, edit func(*Config)) *Server {
	t.Helper()
	cfg := defaultConfig()
	cfg.DefaultProfile = "mock"
	cfg.Profiles["mock"] = Profile{Type: "mock", Model: "mock-1"}
	cfg.Profiles["cheap"] = Profile{Type: "mock", Model: "mock-cheap"}
	cfg.Profiles["nokey"] = Profile{Type: "openai", Model: "x", BaseURL: "https://example.invalid/v1", APIKeyEnv: "TH_TEST_UNSET_KEY"}
	if edit != nil {
		edit(&cfg)
	}
	env := &Env{Workdir: t.TempDir(), Cfg: &cfg, Shell: defaultShell(nil)}
	return &Server{cfg: &cfg, dataDir: t.TempDir(), envs: map[string]*Env{"default": env}, wsNames: []string{"default"},
		hc: newHTTPClient(), sessions: map[string]*Session{}, provs: map[string]Provider{}, modelList: map[string]modelCache{}}
}

func waitIdle(t *testing.T, s *Session) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		s.mu.Lock()
		busy := s.busy
		s.mu.Unlock()
		if !busy {
			return
		}
	}
	t.Fatal("session stayed busy")
}

func compactText(s *Session) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.events {
		if e.Type == "compact" {
			return e.Text
		}
	}
	return ""
}

func oldHistory() []Message {
	big := strings.Repeat("x", 2000)
	return []Message{
		{Role: "user", Content: []Block{{Type: "text", Text: "first request " + big}}},
		{Role: "assistant", Content: []Block{{Type: "text", Text: "first answer " + big}}},
		{Role: "user", Content: []Block{{Type: "text", Text: "second request " + big}}},
		{Role: "assistant", Content: []Block{{Type: "text", Text: "second answer " + big}}},
	}
}

func TestCompactProfileManual(t *testing.T) {
	for profile, want := range map[string]string{"cheap": "(mock-cheap)", "nokey": "(mock-1)", "": "(mock-1)"} {
		sv := testServer(t, func(c *Config) { c.CompactProfile = profile })
		s := newSessionObj(sv, "abcdef01", "mock", "ask", "default")
		s.msgs = oldHistory()
		if err := s.CompactNow(); err != nil {
			t.Fatal(err)
		}
		waitIdle(t, s)
		if got := compactText(s); !strings.Contains(got, want) {
			t.Errorf("compact_profile=%q: summary %q, want model %s", profile, got, want)
		}
	}
}

func TestCompactProfileAuto(t *testing.T) {
	sv := testServer(t, func(c *Config) { c.CompactProfile = "cheap"; c.MaxContextChars = 3000 })
	s := newSessionObj(sv, "abcdef02", "mock", "ask", "default")
	s.msgs = oldHistory()
	if err := s.Send("third request", nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	if got := compactText(s); !strings.Contains(got, "(mock-cheap)") {
		t.Fatalf("auto-compaction summary %q, want the cheap model", got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	last := s.msgs[len(s.msgs)-1]
	if !strings.Contains(last.Content[0].Text, "mock-1") {
		t.Errorf("the chat itself must keep its own model: %q", last.Content[0].Text)
	}
}

func TestExecToolClipsEveryResult(t *testing.T) {
	sv := testServer(t, func(c *Config) { c.MaxToolOutputChars = 2000 })
	env := sv.envs["default"]
	for i := 0; i < 300; i++ {
		_ = os.WriteFile(filepath.Join(env.Workdir, fmt.Sprintf("file-with-a-long-name-%03d.txt", i)), nil, 0o644)
	}
	s := newSessionObj(sv, "abcdef03", "mock", "ask", "default")
	res := s.execTool(context.Background(), env, Block{ID: "t1", Name: "list_dir", Input: json.RawMessage(`{}`)})
	if len(res.Content) > 2000 || !strings.Contains(res.Content, " lines / ") ||
		!strings.HasPrefix(res.Content, "file-with-a-long-name-000.txt") || !strings.Contains(res.Content, "-299.txt") {
		t.Errorf("len=%d content=%q", len(res.Content), res.Content[:80])
	}
}

func TestSystemPromptStable(t *testing.T) {
	sv := testServer(t, nil)
	env := sv.envs["default"]
	a, b := sv.systemPrompt(env, "Monday 26 October 2026"), sv.systemPrompt(env, "Monday 26 October 2026")
	if a != b || !strings.Contains(a, "This chat started on Monday 26 October 2026.") {
		t.Errorf("prompt not stable:\n%s", a)
	}
	j1, _ := json.Marshal(toolDefs())
	j2, _ := json.Marshal(toolDefs())
	if string(j1) != string(j2) {
		t.Error("tool definitions changed between calls")
	}
	// the chat's date is fixed at its first turn and survives a reload
	s := newSessionObj(sv, "abcdef04", "mock", "ask", "default")
	if err := s.Send("hello", nil); err != nil {
		t.Fatal(err)
	}
	waitIdle(t, s)
	s.mu.Lock()
	started := s.started
	s.mu.Unlock()
	if started == "" {
		t.Fatal("started not set")
	}
	delete(sv.sessions, s.ID)
	if again := sv.getSession(s.ID); again == nil || again.started != started {
		t.Errorf("started after reload: %+v", again)
	}
}

func TestMCPOrderIsStable(t *testing.T) {
	exe, _ := os.Executable()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := MCPConfig{Command: exe, Env: map[string]string{"FAKE_MCP": "1"}}
	if n := startAllMCP(ctx, map[string]MCPConfig{"zz": fake, "aa": fake, "mm": fake}); n != 6 {
		t.Fatalf("registered %d tools", n)
	}
	var got []string
	for _, d := range toolDefs() {
		if strings.HasPrefix(d.Name, "mcp__aa__") || strings.HasPrefix(d.Name, "mcp__mm__") || strings.HasPrefix(d.Name, "mcp__zz__") {
			got = append(got, d.Name)
		}
	}
	want := "mcp__aa__echo mcp__aa__fail_now mcp__mm__echo mcp__mm__fail_now mcp__zz__echo mcp__zz__fail_now"
	if strings.Join(got, " ") != want {
		t.Errorf("order: %v", got)
	}
}
