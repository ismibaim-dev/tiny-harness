package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Documented SystemOneResponse shape (api.typesafe.ai/openapi.json).
const jevOK = `{"model":"jev-1.14.0","answers":{
 "destructive":{"type":"noul","noul":0.91},"exfiltrates":{"type":"noul","noul":0.02},
 "installs":{"type":"noul","noul":0.4},"outside_workspace":{"type":"noul","noul":0.05},
 "secrets":{"type":"noul","noul":0.01},"off_task":{"type":"noul","noul":0.1},
 "severity":{"type":"score","score":2.3,"confidence":0.8,"legend":{"0":"Read-only, no effect"},"probabilities":{"0":0.1}}},
 "usage":{"input_tokens":640,"output_tokens":14}}`

func TestRiskLabel(t *testing.T) {
	for in, want := range map[string]string{
		`{"a":{"noul":0.1},"severity":{"score":1.2}}`:  "green",
		`{"a":{"noul":0.35},"b":{"noul":0.2}}`:         "amber",
		`{"a":{"noul":0.5},"b":{"noul":0.7}}`:          "red",
		`{"a":{"noul":0.1},"severity":{"score":2.0}}`:  "red",
		`{"a":{"noul":0.69},"severity":{"score":1.9}}`: "amber",
	} {
		var m map[string]json.RawMessage
		_ = json.Unmarshal([]byte(in), &m)
		if got, _ := riskLabel(m); got != want {
			t.Errorf("riskLabel(%s)=%s want %s", in, got, want)
		}
	}
}

func TestRiskCheckerOff(t *testing.T) {
	cfg := defaultConfig()
	if rc, _ := newRiskChecker(&cfg, http.DefaultClient, t.TempDir()); rc != nil {
		t.Error("should be off without risk_check")
	}
	cfg.RiskCheck = &RiskConfig{APIKeyEnv: "TH_TEST_NO_SUCH_KEY"}
	if rc, note := newRiskChecker(&cfg, http.DefaultClient, t.TempDir()); rc != nil || !strings.Contains(note, "not set") {
		t.Errorf("should be off without key: %q", note)
	}
	var nilRC *riskChecker
	if nilRC.check("s", "default", "/w", "bash", "ls", "hi") != nil {
		t.Error("nil checker must return nil")
	}
}

func testRisk(t *testing.T, h http.HandlerFunc) (*riskChecker, string) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Setenv("TH_TEST_JEV_KEY", "k123")
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.RiskCheck = &RiskConfig{APIKeyEnv: "TH_TEST_JEV_KEY", BaseURL: srv.URL + "/v1", TimeoutMS: 500}
	cfg.RiskSkipWorkspaces = []string{"private"}
	rc, note := newRiskChecker(&cfg, srv.Client(), dir)
	if rc == nil {
		t.Fatal(note)
	}
	return rc, filepath.Join(dir, "risk-shadow.jsonl")
}

func readRiskLines(t *testing.T, path string, n int) []riskLine {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		data, _ := os.ReadFile(path)
		if lines := bytes.Split(bytes.TrimSpace(data), []byte("\n")); len(data) > 0 && len(lines) >= n {
			var out []riskLine
			for _, l := range lines {
				var r riskLine
				if err := json.Unmarshal(l, &r); err != nil {
					t.Fatalf("bad line %s: %v", l, err)
				}
				out = append(out, r)
			}
			return out
		}
	}
	t.Fatalf("expected %d log lines in %s", n, path)
	return nil
}

func TestRiskCheckLogsVerdict(t *testing.T) {
	var got map[string]any
	rc, logPath := testRisk(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Header.Get("Authorization") != "Bearer k123" {
			http.Error(w, "bad", 401)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(jevOK))
	})
	rec := rc.check("sess1", "default", "/work", "bash", "rm -rf build", "clean the build")
	if rec == nil {
		t.Fatal("check should run")
	}
	rec("denied")
	l := readRiskLines(t, logPath, 1)[0]
	if l.Decision != "denied" || l.Label != "red" || l.Tool != "bash" || l.Session != "sess1" || l.Usage["input_tokens"] != 640 {
		t.Errorf("line: %+v", l)
	}
	if strings.Join(l.Reasons, ",") != "destructive,installs,severity" {
		t.Errorf("reasons: %v", l.Reasons)
	}
	st, _ := got["state"].(map[string]any)
	if got["model"] != "jev-1.14.0" || st["user_request"] != "clean the build" || st["action"] != "rm -rf build" || len(got["questions"].(map[string]any)) != 7 {
		t.Errorf("request: %v", got)
	}
	if rc.check("s", "private", "/p", "bash", "ls", "x") != nil {
		t.Error("skipped workspace must not be checked")
	}
}

func TestRiskRetry529(t *testing.T) {
	var n atomic.Int32
	rc, logPath := testRisk(t, func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			http.Error(w, "overloaded", 529)
			return
		}
		w.Write([]byte(jevOK))
	})
	rc.check("s", "default", "/w", "bash", "ls", "x")("allowed")
	if l := readRiskLines(t, logPath, 1)[0]; l.Label != "red" || n.Load() != 2 {
		t.Errorf("label=%s calls=%d", l.Label, n.Load())
	}
}

// A failing or slow Jev must never change what the approval returns.
func TestRiskFailOpen(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"500":     func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) },
		"422":     func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"detail":[]}`, 422) },
		"garbage": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{not json")) },
		"timeout": func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(2 * time.Second):
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			rc, logPath := testRisk(t, h)
			cfg := defaultConfig()
			sv := &Server{cfg: &cfg, risk: rc}
			for i, allow := range []bool{true, false} {
				s := newSessionObj(sv, "abcdef12", "claude", "ask", "default")
				s.msgs = []Message{{Role: "user", Content: []Block{{Type: "text", Text: "do it"}}}}
				id := "call" + string(rune('a'+i))
				done := make(chan bool, 1)
				go func() {
					done <- s.askApproval(context.Background(), &Env{Workdir: "/w"}, Block{ID: id, Name: "bash"}, "make test")
				}()
				for s.Approve(id, allow, false) != nil {
					time.Sleep(5 * time.Millisecond)
				}
				select {
				case got := <-done:
					if got != allow {
						t.Fatalf("approval returned %v, owner said %v", got, allow)
					}
				case <-time.After(time.Second):
					t.Fatal("approval blocked on the risk check")
				}
			}
			for _, l := range readRiskLines(t, logPath, 2) {
				if l.Label != "error" || l.Error == "" {
					t.Errorf("want error line, got %+v", l)
				}
			}
		})
	}
}

func TestRiskNoAnswerAndReport(t *testing.T) {
	rc, logPath := testRisk(t, func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(jevOK)) })
	cfg := defaultConfig()
	sv := &Server{cfg: &cfg, risk: rc}
	s := newSessionObj(sv, "abcdef12", "claude", "ask", "default")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // owner pressed stop before tapping
	if s.askApproval(ctx, &Env{Workdir: "/w"}, Block{ID: "x", Name: "bash"}, "rm -rf /") {
		t.Fatal("cancelled approval must deny")
	}
	rc.check("s", "default", "/w", "bash", "rm -rf build", "x")("allowed")
	ls := readRiskLines(t, logPath, 2)
	decisions := ls[0].Decision + "," + ls[1].Decision
	if decisions != "no_answer,allowed" && decisions != "allowed,no_answer" {
		t.Errorf("decisions: %s", decisions)
	}
	var out bytes.Buffer
	if err := riskReport(&out, logPath); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2 checks") || !strings.Contains(out.String(), "allowed but red (false alarm?) (1)") {
		t.Errorf("report:\n%s", out.String())
	}
}
