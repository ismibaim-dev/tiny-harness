package main

import (
	"context"
	_ "embed"

	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const version = "0.3.0"

//go:embed web/index.html
var indexHTML []byte

type Profile struct {
	Type      string         `json:"type"` // anthropic | openai | gemini | mock
	Model     string         `json:"model"`
	Models    []string       `json:"models,omitempty"` // extra suggestions for the phone's picker
	BaseURL   string         `json:"base_url,omitempty"`
	APIKey    string         `json:"api_key,omitempty"`
	APIKeyEnv string         `json:"api_key_env,omitempty"`
	MaxTokens int            `json:"max_tokens,omitempty"`
	Extra     map[string]any `json:"extra,omitempty"` // merged into every request body
}

type Config struct {
	Listen          string               `json:"listen"`
	Token           string               `json:"token,omitempty"`
	Workdir         string               `json:"workdir"`
	Workspaces      map[string]string    `json:"workspaces,omitempty"` // name -> directory
	DefaultProfile  string               `json:"default_profile"`
	Profiles        map[string]Profile   `json:"profiles"`
	MCPServers      map[string]MCPConfig `json:"mcp_servers,omitempty"`
	Approval        string               `json:"approval"` // ask | auto-edit | yolo
	SafeCommands    []string             `json:"safe_commands"`
	BashTimeoutSec  int                  `json:"bash_timeout_sec"`
	MaxSteps        int                  `json:"max_steps"`
	MaxContextChars int                  `json:"max_context_chars"`
	Shell           []string             `json:"shell,omitempty"`
	NtfyURL         string               `json:"ntfy_url,omitempty"`
	AllowOutside    bool                 `json:"allow_outside_workspace,omitempty"`
	SystemExtra     string               `json:"system_extra,omitempty"`
}

func defaultConfig() Config {
	oa := func(model, base, env string, more ...string) Profile {
		return Profile{Type: "openai", Model: model, BaseURL: base, APIKeyEnv: env, Models: more}
	}
	return Config{
		Listen:         "127.0.0.1:8787",
		Workdir:        ".",
		DefaultProfile: "claude",
		Profiles: map[string]Profile{
			"claude":     {Type: "anthropic", Model: "claude-sonnet-5-5", APIKeyEnv: "ANTHROPIC_API_KEY", Models: []string{"claude-opus-5-5", "claude-haiku-5-5"}},
			"gemini":     {Type: "gemini", Model: "gemini-3.8-flash", APIKeyEnv: "GEMINI_API_KEY", Models: []string{"gemini-4-argon", "gemini-3.6-flash", "gemini-3.5-flash-lite"}},
			"deepseek":   oa("deepseek-flash", "https://api.deepseek.com", "DEEPSEEK_API_KEY", "deepseek-v4-pro"),
			"glm":        oa("glm-4.6", "https://api.z.ai/api/paas/v4", "ZAI_API_KEY", "glm-5"),
			"openai":     oa("gpt-5", "https://api.openai.com/v1", "OPENAI_API_KEY"),
			"openrouter": oa("openrouter/auto", "https://openrouter.ai/api/v1", "OPENROUTER_API_KEY"),
			"groq":       oa("openai/gpt-oss-120b", "https://api.groq.com/openai/v1", "GROQ_API_KEY"),
			"mistral":    oa("mistral-large-latest", "https://api.mistral.ai/v1", "MISTRAL_API_KEY"),
		},
		Approval:        "ask",
		SafeCommands:    []string{"pwd", "whoami", "date", "git status", "git log", "git diff", "go version"},
		BashTimeoutSec:  120,
		MaxSteps:        60,
		MaxContextChars: 400000,
	}
}

func dataDir() string {
	if d := os.Getenv("TINY_HARNESS_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".tiny-harness")
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "") + "…"
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func loadConfig(path string, explicit bool) (Config, error) {
	cfg := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) && !explicit {
			return cfg, nil
		}
		return cfg, err
	}
	// Profiles in the file are added to (or replace) the built-in presets.
	if err := json.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

func loadToken(cfg Config, dir string) (token, note string, err error) {
	if cfg.Token != "" {
		return cfg.Token, "token from config", nil
	}
	path := filepath.Join(dir, "token")
	if b, err := os.ReadFile(path); err == nil && len(strings.TrimSpace(string(b))) >= 16 {
		return strings.TrimSpace(string(b)), "token file: " + path, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", err
	}
	t := randHex(24)
	if err := os.WriteFile(path, []byte(t+"\n"), 0o600); err != nil {
		return "", "", err
	}
	return t, "NEW token (also saved to " + path + "):\n\n    " + t + "\n", nil
}

func mustDir(p string) string {
	abs, err := filepath.Abs(os.ExpandEnv(p))
	if err != nil {
		log.Fatal(err)
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		log.Fatalf("workspace %s is not a directory", abs)
	}
	return abs
}

func main() {
	cfgPath := flag.String("config", "", "config file (default: ./harness.json, then <data dir>/config.json)")
	listen := flag.String("listen", "", "address to listen on, e.g. 127.0.0.1:8787")
	workdir := flag.String("workdir", "", "default workspace directory")
	approval := flag.String("approval", "", "ask | auto-edit | yolo")
	initCfg := flag.Bool("init", false, "write an example config to the data dir and exit")
	showVer := flag.Bool("version", false, "print version")
	flag.Parse()
	if *showVer {
		fmt.Println("tiny-harness", version)
		return
	}

	dir := dataDir()
	if *initCfg {
		p := filepath.Join(dir, "config.json")
		if _, err := os.Stat(p); err == nil {
			log.Fatalf("%s already exists", p)
		}
		_ = os.MkdirAll(dir, 0o700)
		ex := defaultConfig()
		ex.Workspaces = map[string]string{"example": "C:\\path\\to\\another\\project"}
		ex.MCPServers = map[string]MCPConfig{}
		data, _ := json.MarshalIndent(ex, "", "  ")
		if err := os.WriteFile(p, data, 0o600); err != nil {
			log.Fatal(err)
		}
		fmt.Println("wrote", p)
		return
	}

	explicit := *cfgPath != ""
	path := *cfgPath
	if path == "" {
		path = "harness.json"
		if _, err := os.Stat(path); err != nil {
			path = filepath.Join(dir, "config.json")
		}
	}
	cfg, err := loadConfig(path, explicit)
	if err != nil {
		log.Fatal(err)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	if *workdir != "" {
		cfg.Workdir = *workdir
	}
	if *approval != "" {
		cfg.Approval = *approval
	}
	switch cfg.Approval {
	case "ask", "auto-edit", "yolo":
	default:
		log.Fatalf("approval must be ask, auto-edit or yolo (got %q)", cfg.Approval)
	}
	if _, ok := cfg.Profiles[cfg.DefaultProfile]; !ok {
		log.Fatalf("default_profile %q is not defined in profiles", cfg.DefaultProfile)
	}
	if cfg.BashTimeoutSec <= 0 {
		cfg.BashTimeoutSec = 120
	}
	if cfg.MaxSteps <= 0 {
		cfg.MaxSteps = 60
	}
	if cfg.MaxContextChars <= 0 {
		cfg.MaxContextChars = 400000
	}
	token, note, err := loadToken(cfg, dir)
	if err != nil {
		log.Fatal(err)
	}

	shell := defaultShell(cfg.Shell)
	envs := map[string]*Env{"default": {Workdir: mustDir(cfg.Workdir), Cfg: &cfg, Shell: shell}}
	names := []string{"default"}
	var extra []string
	for n, p := range cfg.Workspaces {
		if n == "default" || !validWSName(n) {
			log.Fatalf("bad workspace name %q (letters, digits, - and _ only; not \"default\")", n)
		}
		envs[n] = &Env{Workdir: mustDir(p), Cfg: &cfg, Shell: shell}
		extra = append(extra, n)
	}
	sort.Strings(extra)
	names = append(names, extra...)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	sv := &Server{
		cfg: &cfg, token: token, dataDir: dir, envs: envs, wsNames: names,
		hc:        newHTTPClient(),
		sessions:  map[string]*Session{},
		provs:     map[string]Provider{},
		modelList: map[string]modelCache{},
	}
	if len(cfg.MCPServers) > 0 {
		sv.mcpN = startAllMCP(ctx, cfg.MCPServers)
	}
	go sv.janitor()

	fmt.Printf("tiny-harness %s  http://%s/\n", version, cfg.Listen)
	for _, n := range names {
		fmt.Printf("  workspace : %-10s %s\n", n, envs[n].Workdir)
	}
	fmt.Printf("  shell     : %s\n  approval  : %s\n  models    : %s\n  tools     : %d (%d from MCP)\n  config    : %s\n  %s\n",
		shellName(shell), cfg.Approval, orNone(sv.usableProfiles()), len(tools), sv.mcpN, path, note)
	if len(sv.usableProfiles()) == 0 {
		fmt.Println("  ! no usable model: set an API key env var (ANTHROPIC_API_KEY, GEMINI_API_KEY, DEEPSEEK_API_KEY, ...) or edit the config")
	}
	if !strings.HasPrefix(cfg.Listen, "127.0.0.1") && !strings.HasPrefix(cfg.Listen, "localhost") && !strings.HasPrefix(cfg.Listen, "[::1]") {
		fmt.Println("  ! listening beyond loopback: only do this on a private network (Tailscale); this is a remote shell")
	}

	srv := &http.Server{Addr: cfg.Listen, Handler: sv.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sv.mu.Lock()
		for _, s := range sv.sessions {
			s.save()
		}
		sv.mu.Unlock()
		sctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func validWSName(n string) bool {
	if n == "" || len(n) > 40 {
		return false
	}
	for _, r := range n {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func orNone(n []string) string {
	if len(n) == 0 {
		return "(none)"
	}
	return strings.Join(n, ", ")
}
