package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Native Gemini API (generateContent / streamGenerateContent).
type geminiProv struct {
	name string
	p    Profile
	key  string
	hc   *http.Client
}

// Accepted by Gemini 3 in place of a real thought signature for function calls
// that did not come from this model (history from another model, or old sessions).
const geminiDummySig = "skip_thought_signature_validator"

func (g *geminiProv) base() string {
	if b := strings.TrimRight(g.p.BaseURL, "/"); b != "" {
		return b
	}
	return "https://generativelanguage.googleapis.com"
}

func (g *geminiProv) headers() map[string]string { return map[string]string{"x-goog-api-key": g.key} }

func geminiModelPath(m string) string {
	if strings.HasPrefix(m, "models/") || strings.HasPrefix(m, "tunedModels/") {
		return m
	}
	return "models/" + m
}

// geminiSchema keeps only the OpenAPI-subset keys Gemini's `parameters` accepts.
// MCP servers often send full JSON Schema ($schema, additionalProperties, type
// arrays...), which Gemini rejects with a 400.
func geminiSchema(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for k, val := range m {
		switch k {
		case "type":
			switch t := val.(type) {
			case string:
				out["type"] = t
			case []any:
				for _, x := range t {
					if s, _ := x.(string); s == "null" {
						out["nullable"] = true
					} else if s != "" && out["type"] == nil {
						out["type"] = s
					}
				}
			}
		case "description", "format", "title", "pattern", "nullable",
			"minimum", "maximum", "minLength", "maxLength", "minItems", "maxItems",
			"minProperties", "maxProperties", "propertyOrdering":
			out[k] = val
		case "enum":
			if arr, ok := val.([]any); ok {
				var ss []any
				for _, x := range arr {
					if s, ok := x.(string); ok {
						ss = append(ss, s)
					}
				}
				if len(ss) == len(arr) && len(ss) > 0 {
					out["enum"] = ss
				}
			}
		case "const":
			if s, ok := val.(string); ok {
				out["enum"] = []any{s}
			}
		case "items":
			out["items"] = geminiSchema(val)
		case "anyOf", "oneOf":
			if arr, ok := val.([]any); ok {
				var subs []any
				for _, x := range arr {
					subs = append(subs, geminiSchema(x))
				}
				out["anyOf"] = subs
			}
		case "properties":
			if props, ok := val.(map[string]any); ok && len(props) > 0 {
				p := map[string]any{}
				for pk, pv := range props {
					p[pk] = geminiSchema(pv)
				}
				out["properties"] = p
			}
		}
	}
	if req, ok := m["required"].([]any); ok {
		if props, ok := out["properties"].(map[string]any); ok {
			var keep []any
			for _, r := range req {
				if s, ok := r.(string); ok && props[s] != nil {
					keep = append(keep, s)
				}
			}
			if len(keep) > 0 {
				out["required"] = keep
			}
		}
	} else if req, ok := m["required"].([]string); ok && len(req) > 0 {
		out["required"] = req
	}
	if out["type"] == nil && out["anyOf"] == nil {
		if out["properties"] != nil {
			out["type"] = "object"
		} else {
			out["type"] = "string"
		}
	}
	return out
}

// normalize round-trips a Go schema literal through JSON so geminiSchema sees []any.
func normalize(s map[string]any) map[string]any {
	raw, _ := json.Marshal(s)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return m
}

func toGemini(msgs []Message, src string) []map[string]any {
	names := map[string]string{}
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		role := "user"
		if m.Role == "assistant" {
			role = "model"
		}
		var parts []map[string]any
		firstCall := true
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				p := map[string]any{"text": b.Text}
				if b.Sig != "" && b.Src == src {
					p["thoughtSignature"] = b.Sig
				}
				if b.Text != "" || p["thoughtSignature"] != nil {
					parts = append(parts, p)
				}
			case "image":
				parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": b.Media, "data": b.Data}})
			case "tool_use":
				names[b.ID] = b.Name
				var args any = map[string]any{}
				if len(b.Input) > 0 {
					_ = json.Unmarshal(b.Input, &args)
				}
				fc := map[string]any{"name": b.Name, "args": args}
				if !strings.HasPrefix(b.ID, "gem_") {
					fc["id"] = b.ID
				}
				p := map[string]any{"functionCall": fc}
				switch {
				case b.Sig != "" && b.Src == src:
					p["thoughtSignature"] = b.Sig
				case firstCall:
					p["thoughtSignature"] = geminiDummySig
				}
				firstCall = false
				parts = append(parts, p)
			case "tool_result":
				key := "output"
				if b.IsError {
					key = "error"
				}
				fr := map[string]any{"name": names[b.ToolUseID], "response": map[string]any{key: b.Content}}
				if !strings.HasPrefix(b.ToolUseID, "gem_") {
					fr["id"] = b.ToolUseID
				}
				parts = append(parts, map[string]any{"functionResponse": fr})
			}
		}
		if len(parts) == 0 {
			parts = []map[string]any{{"text": "(empty)"}}
		}
		out = append(out, map[string]any{"role": role, "parts": parts})
	}
	return out
}

func (g *geminiProv) Complete(ctx context.Context, r Request) (Response, error) {
	src := g.name + "/" + r.Model
	maxTok := g.p.MaxTokens
	if maxTok <= 0 {
		maxTok = 32768 // thinking tokens share this budget
	}
	body := map[string]any{
		"systemInstruction": map[string]any{"parts": []map[string]any{{"text": r.System}}},
		"contents":          toGemini(r.Msgs, src),
		"generationConfig":  map[string]any{"maxOutputTokens": maxTok},
	}
	if len(r.Tools) > 0 {
		decls := make([]map[string]any, 0, len(r.Tools))
		for _, t := range r.Tools {
			d := map[string]any{"name": t.Name, "description": t.Description}
			if s, ok := geminiSchema(normalize(t.Schema)).(map[string]any); ok && s["properties"] != nil {
				d["parameters"] = s
			}
			decls = append(decls, d)
		}
		body["tools"] = []map[string]any{{"functionDeclarations": decls}}
	}
	merge(body, g.p.Extra)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	url := g.base() + "/v1beta/" + geminiModelPath(r.Model) + ":streamGenerateContent?alt=sse"
	resp, err := send(ctx, g.hc, "POST", url, g.headers(), body)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	var res Response
	var blocks []Block
	textIdx := -1 // index in blocks of the text block being streamed
	blockReason := ""
	err = readSSE(ctx, cancel, resp.Body, func(_, data string) error {
		var c struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text             string `json:"text"`
						Thought          bool   `json:"thought"`
						ThoughtSignature string `json:"thoughtSignature"`
						FunctionCall     *struct {
							ID   string          `json:"id"`
							Name string          `json:"name"`
							Args json.RawMessage `json:"args"`
						} `json:"functionCall"`
					} `json:"parts"`
				} `json:"content"`
				FinishReason string `json:"finishReason"`
			} `json:"candidates"`
			UsageMetadata *struct {
				Prompt   int `json:"promptTokenCount"`
				Output   int `json:"candidatesTokenCount"`
				Thoughts int `json:"thoughtsTokenCount"`
				Cached   int `json:"cachedContentTokenCount"`
			} `json:"usageMetadata"`
			PromptFeedback struct {
				BlockReason string `json:"blockReason"`
			} `json:"promptFeedback"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &c) != nil {
			return nil
		}
		if c.Error != nil {
			return fmt.Errorf("gemini: %s", c.Error.Message)
		}
		if u := c.UsageMetadata; u != nil {
			res.Usage = Usage{In: u.Prompt, Out: u.Output + u.Thoughts, CacheRead: u.Cached}
		}
		if c.PromptFeedback.BlockReason != "" {
			blockReason = c.PromptFeedback.BlockReason
		}
		for _, cand := range c.Candidates {
			for _, p := range cand.Content.Parts {
				switch {
				case p.FunctionCall != nil:
					id := p.FunctionCall.ID
					if id == "" {
						id = "gem_" + randHex(6)
					}
					b := Block{Type: "tool_use", ID: id, Name: p.FunctionCall.Name, Input: p.FunctionCall.Args}
					if p.ThoughtSignature != "" {
						b.Sig, b.Src = p.ThoughtSignature, src
					}
					blocks = append(blocks, b)
					textIdx = -1
				case p.Thought:
					// thought summaries are only sent with includeThoughts; not shown
				default:
					if textIdx < 0 {
						blocks = append(blocks, Block{Type: "text"})
						textIdx = len(blocks) - 1
					}
					blocks[textIdx].Text += p.Text
					if p.ThoughtSignature != "" {
						blocks[textIdx].Sig, blocks[textIdx].Src = p.ThoughtSignature, src
					}
					if p.Text != "" && r.OnText != nil {
						r.OnText(p.Text)
					}
				}
			}
			switch cand.FinishReason {
			case "MAX_TOKENS":
				res.Stop = "max_tokens"
			case "SAFETY", "RECITATION", "PROHIBITED_CONTENT", "BLOCKLIST", "SPII":
				blockReason = cand.FinishReason
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	for _, b := range blocks {
		// keep empty text blocks only if they carry a signature (echoed back, not shown)
		if b.Type == "text" && strings.TrimSpace(b.Text) == "" && b.Sig == "" {
			continue
		}
		res.Blocks = append(res.Blocks, b)
	}
	if len(res.Blocks) == 0 && blockReason != "" {
		return res, fmt.Errorf("gemini blocked the response (%s)", blockReason)
	}
	res.Blocks = cleanBlocks(res.Blocks)
	return res, nil
}

func (g *geminiProv) Models(ctx context.Context) ([]string, error) {
	var out struct {
		Models []struct {
			Name    string   `json:"name"`
			Methods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := getJSON(ctx, g.hc, g.base()+"/v1beta/models?pageSize=200", g.headers(), &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, m := range out.Models {
		for _, x := range m.Methods {
			if x == "generateContent" {
				ids = append(ids, strings.TrimPrefix(m.Name, "models/"))
				break
			}
		}
	}
	return ids, nil
}
