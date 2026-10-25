package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// fakeSSE serves a canned SSE stream and records the request body.
func fakeSSE(t *testing.T, events []string, got *map[string]any, path *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if got != nil {
			_ = json.Unmarshal(raw, got)
		}
		if path != nil {
			*path = r.URL.String() + " " + r.Header.Get("x-goog-api-key") + r.Header.Get("x-api-key") + r.Header.Get("Authorization")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			fmt.Fprintf(w, "%s\n\n", e)
			w.(http.Flusher).Flush()
		}
	}))
}

var history = []Message{
	{Role: "user", Content: []Block{{Type: "text", Text: "look"}, {Type: "image", Media: "image/png", Data: "AAAA"}}},
	{Role: "assistant", Content: []Block{
		{Type: "reasoning", Text: "thinking...", Name: "reasoning_content", Src: "ds/deepseek-flash"},
		{Type: "text", Text: "ok"},
		{Type: "tool_use", ID: "c1", Name: "bash", Input: json.RawMessage(`{"command":"pwd"}`), Sig: "SIG1", Src: "gm/gemini-3.8-flash"},
	}},
	{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "c1", Content: "/x"}}},
}

func TestAnthropicStream(t *testing.T) {
	var body map[string]any
	srv := fakeSSE(t, []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":90,"output_tokens":1}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"bash"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"command\":"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"ls\"}"}}`,
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}`,
		`data: {"type":"message_stop"}`,
	}, &body, nil)
	defer srv.Close()
	p := &anthropicProv{name: "c", p: Profile{BaseURL: srv.URL}, key: "k", hc: newHTTPClient()}
	var streamed strings.Builder
	res, err := p.Complete(context.Background(), Request{System: "sys", Msgs: history, Tools: toolDefs(), Model: "m",
		OnText: func(s string) { streamed.WriteString(s) }})
	if err != nil {
		t.Fatal(err)
	}
	if streamed.String() != "Hello" || len(res.Blocks) != 2 || res.Blocks[1].Name != "bash" || string(res.Blocks[1].Input) != `{"command":"ls"}` {
		t.Errorf("parse: %q %+v", streamed.String(), res.Blocks)
	}
	if res.Usage.In != 100 || res.Usage.Out != 42 || res.Usage.CacheRead != 90 {
		t.Errorf("usage: %+v", res.Usage)
	}
	raw, _ := json.Marshal(body)
	s := string(raw)
	for _, want := range []string{`"cache_control":{"type":"ephemeral"}`, `"type":"image"`, `"media_type":"image/png"`, `"tool_use_id":"c1"`} {
		if !strings.Contains(s, want) {
			t.Errorf("request missing %s", want)
		}
	}
	for _, bad := range []string{"reasoning", "SIG1", `"sig"`} {
		if strings.Contains(s, bad) {
			t.Errorf("request must not contain %s", bad)
		}
	}
}

func TestOpenAIStreamAndReasoningEcho(t *testing.T) {
	var body map[string]any
	srv := fakeSSE(t, []string{
		`data: {"choices":[{"delta":{"reasoning_content":"hmm "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"ok"}}]}`,
		`data: {"choices":[{"delta":{"content":"Hi"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.go\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"list_dir","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":50,"completion_tokens":7,"prompt_cache_hit_tokens":40}}`,
		`data: [DONE]`,
	}, &body, nil)
	defer srv.Close()
	p := &openaiProv{name: "ds", p: Profile{BaseURL: srv.URL}, key: "k", hc: newHTTPClient()}
	res, err := p.Complete(context.Background(), Request{System: "sys", Msgs: history, Tools: toolDefs(), Model: "deepseek-flash"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Blocks) != 4 || res.Blocks[0].Type != "reasoning" || res.Blocks[0].Text != "hmm ok" ||
		res.Blocks[2].ID != "call_a" || string(res.Blocks[2].Input) != `{"path":"a.go"}` || res.Blocks[3].Name != "list_dir" {
		t.Errorf("parse: %+v", res.Blocks)
	}
	if res.Usage.In != 50 || res.Usage.CacheRead != 40 {
		t.Errorf("usage %+v", res.Usage)
	}
	msgs := body["messages"].([]any)
	asst := msgs[2].(map[string]any)
	if asst["reasoning_content"] != "thinking..." {
		t.Errorf("reasoning_content not echoed to same profile: %+v", asst)
	}
	user := msgs[1].(map[string]any)
	if _, isArr := user["content"].([]any); !isArr {
		t.Errorf("image message should use content parts: %+v", user)
	}
	if body["stream_options"] == nil {
		t.Error("expected stream_options")
	}
	// a different profile must not receive DeepSeek's reasoning
	out := toOpenAI("s", history, "other/x")
	if _, ok := out[2]["reasoning_content"]; ok {
		t.Error("reasoning leaked to another profile")
	}
}

func TestOpenAIStreamOptionsFallback(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "stream_options") {
			http.Error(w, `{"error":"unknown field stream_options"}`, 400)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	p := &openaiProv{name: "x", p: Profile{BaseURL: srv.URL}, key: "k", hc: newHTTPClient()}
	res, err := p.Complete(context.Background(), Request{System: "s", Msgs: history[:1], Model: "m"})
	if err != nil || len(res.Blocks) != 1 || calls != 2 {
		t.Fatalf("fallback: %v %+v calls=%d", err, res.Blocks, calls)
	}
	_, _ = p.Complete(context.Background(), Request{System: "s", Msgs: history[:1], Model: "m"})
	if calls != 3 {
		t.Errorf("should remember the fallback, calls=%d", calls)
	}
}

func TestGeminiStream(t *testing.T) {
	var body map[string]any
	var path string
	srv := fakeSSE(t, []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Let me "}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"check."}]}}]}`,
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"grep","args":{"pattern":"TODO"}},"thoughtSignature":"NEWSIG"},{"functionCall":{"name":"list_dir","args":{}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":30,"thoughtsTokenCount":70,"cachedContentTokenCount":150}}`,
	}, &body, &path)
	defer srv.Close()
	p := &geminiProv{name: "gm", p: Profile{BaseURL: srv.URL}, key: "gk", hc: newHTTPClient()}
	res, err := p.Complete(context.Background(), Request{System: "sys", Msgs: history, Tools: toolDefs(), Model: "gemini-3.8-flash"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(path, "/v1beta/models/gemini-3.8-flash:streamGenerateContent?alt=sse gk") {
		t.Errorf("url/key: %s", path)
	}
	if len(res.Blocks) != 3 || res.Blocks[0].Text != "Let me check." || res.Blocks[1].Sig != "NEWSIG" ||
		res.Blocks[1].Src != "gm/gemini-3.8-flash" || res.Blocks[2].Sig != "" || res.Usage.Out != 100 {
		t.Errorf("parse: %+v %+v", res.Blocks, res.Usage)
	}
	raw, _ := json.Marshal(body)
	s := string(raw)
	for _, want := range []string{`"thoughtSignature":"SIG1"`, `"functionResponse":{"id":"c1","name":"bash","response":{"output":"/x"}}`,
		`"inlineData":{"data":"AAAA","mimeType":"image/png"}`, `"role":"model"`, `"systemInstruction"`} {
		if !strings.Contains(s, want) {
			t.Errorf("request missing %s\n%s", want, trunc(s, 3000))
		}
	}
	// history from another model gets the dummy signature on its first call only
	other := toGemini(history, "gm/another-model")
	fc := other[1]["parts"].([]map[string]any)
	if fc[len(fc)-1]["thoughtSignature"] != geminiDummySig {
		t.Errorf("expected dummy signature: %+v", fc)
	}
}

func TestGeminiSchemaSanitizer(t *testing.T) {
	in := map[string]any{
		"$schema": "http://json-schema.org/draft-07/schema#", "type": "object", "additionalProperties": false,
		"properties": map[string]any{
			"q":    map[string]any{"type": []any{"string", "null"}, "description": "query", "default": "x"},
			"mode": map[string]any{"const": "fast"},
			"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string", "examples": []any{"a"}}},
			"n":    map[string]any{"type": "integer", "enum": []any{1, 2}},
		},
		"required": []any{"q", "ghost"},
	}
	out, _ := json.Marshal(geminiSchema(in))
	s := string(out)
	for _, bad := range []string{"$schema", "additionalProperties", "default", "examples", "ghost", `"enum":[1`} {
		if strings.Contains(s, bad) {
			t.Errorf("sanitized schema still has %s: %s", bad, s)
		}
	}
	for _, want := range []string{`"nullable":true`, `"type":"string"`, `"enum":["fast"]`, `"required":["q"]`} {
		if !strings.Contains(s, want) {
			t.Errorf("sanitized schema missing %s: %s", want, s)
		}
	}
	// every built-in tool must survive sanitizing
	for _, d := range toolDefs() {
		if _, err := json.Marshal(geminiSchema(normalize(d.Schema))); err != nil {
			t.Error(d.Name, err)
		}
	}
}

func TestRepairHistory(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: []Block{{Type: "text", Text: "hi"}}},
		{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "a"}, {Type: "tool_use", ID: "b"}}},
		{Role: "user", Content: []Block{{Type: "tool_result", ToolUseID: "a"}}},
		{Role: "assistant", Content: []Block{{Type: "tool_use", ID: "c"}}},
	}
	out := repairHistory(msgs)
	if len(out) != 5 || len(out[2].Content) != 2 || out[4].Content[0].ToolUseID != "c" {
		t.Errorf("repair: %+v", out)
	}
}

// ---------------------------------------------------------------- MCP

// TestMain lets the test binary double as a tiny MCP server.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_MCP") == "1" {
		fakeMCPServer()
		return
	}
	os.Exit(m.Run())
}

func fakeMCPServer() {
	sc := bufio.NewScanner(os.Stdin)
	out := json.NewEncoder(os.Stdout)
	fmt.Println("log noise on stdout that is not JSON")
	for sc.Scan() {
		var req struct {
			ID     *int64         `json:"id"`
			Method string         `json:"method"`
			Params map[string]any `json:"params"`
		}
		if json.Unmarshal(sc.Bytes(), &req) != nil || req.ID == nil {
			continue
		}
		var result any
		switch req.Method {
		case "initialize":
			// also send a server->client request the client must answer
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": "srv1", "method": "roots/list"})
			result = map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fake"}}
		case "tools/list":
			if req.Params["cursor"] == nil {
				result = map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Echo text",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}}}}, "nextCursor": "p2"}
			} else {
				result = map[string]any{"tools": []any{map[string]any{"name": "fail.now", "inputSchema": map[string]any{"type": "object"}}}}
			}
		case "tools/call":
			args, _ := req.Params["arguments"].(map[string]any)
			if req.Params["name"] == "fail.now" {
				result = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "it broke"}}}
			} else {
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": fmt.Sprint("echo: ", args["text"])}}}
			}
		default:
			out.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "error": map[string]any{"code": -32601, "message": "nope"}})
			continue
		}
		out.Encode(map[string]any{"jsonrpc": "2.0", "id": *req.ID, "result": result})
	}
}

func TestMCP(t *testing.T) {
	exe, _ := os.Executable()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := startAllMCP(ctx, map[string]MCPConfig{"fake": {Command: exe, Env: map[string]string{"FAKE_MCP": "1"}, AutoApprove: []string{"echo"}}})
	if n != 2 {
		t.Fatalf("registered %d tools", n)
	}
	echo := toolByName["mcp__fake__echo"]
	fail := toolByName["mcp__fake__fail_now"]
	if echo == nil || fail == nil {
		t.Fatal("tools not registered")
	}
	if echo.Kind != "read" || fail.Kind != "exec" {
		t.Errorf("auto-approve kinds: %s %s", echo.Kind, fail.Kind)
	}
	out, err := echo.Run(ctx, nil, json.RawMessage(`{"text":"hi"}`))
	if err != nil || !strings.Contains(out, "echo: hi") {
		t.Errorf("call: %q %v", out, err)
	}
	if _, err := fail.Run(ctx, nil, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "it broke") {
		t.Errorf("isError should become an error: %v", err)
	}
}
