package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
)

// OpenAI-compatible /chat/completions: OpenAI, DeepSeek, GLM, Kimi, Qwen,
// Groq, Mistral, xAI, OpenRouter, Gemini's compat endpoint, Ollama, LM Studio...
type openaiProv struct {
	name    string
	p       Profile
	key     string
	hc      *http.Client
	noUsage atomic.Bool // provider rejected stream_options
}

func (o *openaiProv) base() string {
	if b := strings.TrimRight(o.p.BaseURL, "/"); b != "" {
		return b
	}
	return "https://api.openai.com/v1"
}

func (o *openaiProv) headers() map[string]string {
	h := map[string]string{}
	if o.key != "" {
		h["Authorization"] = "Bearer " + o.key
	}
	return h
}

// toOpenAI converts history. src is "profile/model": reasoning and
// extra_content are only echoed back to the profile that produced them
// (DeepSeek thinking mode returns 400 if reasoning_content is dropped).
func toOpenAI(system string, msgs []Message, src string) []map[string]any {
	prof := strings.SplitN(src, "/", 2)[0] + "/"
	out := []map[string]any{{"role": "system", "content": system}}
	for _, m := range msgs {
		if m.Role == "assistant" {
			msg := map[string]any{"role": "assistant"}
			var texts []string
			var calls []map[string]any
			for _, b := range m.Content {
				switch b.Type {
				case "text":
					texts = append(texts, b.Text)
				case "reasoning":
					if strings.HasPrefix(b.Src, prof) && b.Name != "" {
						msg[b.Name] = b.Text
					}
				case "tool_use":
					args := string(b.Input)
					if args == "" {
						args = "{}"
					}
					c := map[string]any{"id": b.ID, "type": "function",
						"function": map[string]any{"name": b.Name, "arguments": args}}
					if b.Sig != "" && strings.HasPrefix(b.Src, prof) && json.Valid([]byte(b.Sig)) {
						c["extra_content"] = json.RawMessage(b.Sig)
					}
					calls = append(calls, c)
				}
			}
			content := strings.Join(texts, "\n")
			msg["content"] = content
			if len(calls) > 0 {
				msg["tool_calls"] = calls
				if content == "" {
					msg["content"] = nil
				}
			}
			out = append(out, msg)
			continue
		}
		texts, imgs := imageBlocks(m)
		for _, b := range m.Content {
			if b.Type == "tool_result" {
				out = append(out, map[string]any{"role": "tool", "tool_call_id": b.ToolUseID, "content": b.Content})
			}
		}
		switch {
		case len(imgs) > 0:
			parts := []map[string]any{}
			if t := strings.Join(texts, "\n"); t != "" {
				parts = append(parts, map[string]any{"type": "text", "text": t})
			}
			for _, im := range imgs {
				parts = append(parts, map[string]any{"type": "image_url",
					"image_url": map[string]any{"url": "data:" + im.Media + ";base64," + im.Data}})
			}
			out = append(out, map[string]any{"role": "user", "content": parts})
		case len(texts) > 0:
			out = append(out, map[string]any{"role": "user", "content": strings.Join(texts, "\n")})
		}
	}
	return out
}

func (o *openaiProv) Complete(ctx context.Context, r Request) (Response, error) {
	tdefs := make([]map[string]any, 0, len(r.Tools))
	for _, t := range r.Tools {
		tdefs = append(tdefs, map[string]any{"type": "function",
			"function": map[string]any{"name": t.Name, "description": t.Description, "parameters": t.Schema}})
	}
	src := o.name + "/" + r.Model
	body := map[string]any{
		"model":    r.Model,
		"messages": toOpenAI(r.System, r.Msgs, src),
		"stream":   true,
	}
	if len(tdefs) > 0 {
		body["tools"] = tdefs
	}
	if o.p.MaxTokens > 0 {
		body["max_tokens"] = o.p.MaxTokens
	}
	if !o.noUsage.Load() {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	merge(body, o.p.Extra)

	res, err := o.stream(ctx, body, r, src)
	var he *httpError
	if err != nil && errors.As(err, &he) && he.Status == 400 && strings.Contains(he.Body, "stream_options") {
		o.noUsage.Store(true)
		delete(body, "stream_options")
		res, err = o.stream(ctx, body, r, src)
	}
	return res, err
}

func (o *openaiProv) stream(ctx context.Context, body map[string]any, r Request, src string) (Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := send(ctx, o.hc, "POST", o.base()+"/chat/completions", o.headers(), body)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	type call struct {
		id, name string
		args     strings.Builder
		extra    json.RawMessage
	}
	var calls []*call
	byIndex := map[int]*call{}
	var text, reasoning strings.Builder
	reasonKey := ""
	var res Response

	err = readSSE(ctx, cancel, resp.Body, func(_, data string) error {
		if data == "[DONE]" {
			return nil
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
					Reasoning        string `json:"reasoning"`
					ToolCalls        []struct {
						Index    *int   `json:"index"`
						ID       string `json:"id"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
						ExtraContent json.RawMessage `json:"extra_content"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				Prompt     int `json:"prompt_tokens"`
				Completion int `json:"completion_tokens"`
				Details    struct {
					Cached int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
				CacheHit int `json:"prompt_cache_hit_tokens"`
			} `json:"usage"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &c) != nil {
			return nil
		}
		if c.Error != nil {
			return fmt.Errorf("provider error: %s", c.Error.Message)
		}
		if u := c.Usage; u != nil {
			res.Usage = Usage{In: u.Prompt, Out: u.Completion, CacheRead: max(u.Details.Cached, u.CacheHit)}
		}
		for _, ch := range c.Choices {
			d := ch.Delta
			if d.Content != "" {
				text.WriteString(d.Content)
				if r.OnText != nil {
					r.OnText(d.Content)
				}
			}
			if d.ReasoningContent != "" {
				reasoning.WriteString(d.ReasoningContent)
				reasonKey = "reasoning_content"
			} else if d.Reasoning != "" {
				reasoning.WriteString(d.Reasoning)
				if reasonKey == "" {
					reasonKey = "reasoning"
				}
			}
			for i, tc := range d.ToolCalls {
				idx := i
				if tc.Index != nil {
					idx = *tc.Index
				}
				cur := byIndex[idx]
				if cur == nil || (tc.ID != "" && cur.id != "" && tc.ID != cur.id) {
					cur = &call{}
					byIndex[idx] = cur
					calls = append(calls, cur)
				}
				if tc.ID != "" {
					cur.id = tc.ID
				}
				if tc.Function.Name != "" {
					cur.name = tc.Function.Name
				}
				cur.args.WriteString(tc.Function.Arguments)
				if len(tc.ExtraContent) > 0 && string(tc.ExtraContent) != "null" {
					cur.extra = tc.ExtraContent
				}
			}
			if ch.FinishReason == "length" {
				res.Stop = "max_tokens"
			}
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	if reasoning.Len() > 0 {
		// Echoed back as "reasoning_content" (DeepSeek, GLM, Kimi); other names are kept for display only.
		key := ""
		if reasonKey == "reasoning_content" {
			key = reasonKey
		}
		res.Blocks = append(res.Blocks, Block{Type: "reasoning", Text: reasoning.String(), Name: key, Src: src})
	}
	if t := text.String(); strings.TrimSpace(t) != "" {
		res.Blocks = append(res.Blocks, Block{Type: "text", Text: t})
	}
	for _, c := range calls {
		if c.name == "" {
			continue
		}
		id := c.id
		if id == "" {
			id = "call_" + randHex(6)
		}
		b := Block{Type: "tool_use", ID: id, Name: c.name, Input: json.RawMessage(strings.TrimSpace(c.args.String()))}
		if len(c.extra) > 0 {
			b.Sig, b.Src = string(c.extra), src
		}
		res.Blocks = append(res.Blocks, b)
	}
	res.Blocks = cleanBlocks(res.Blocks)
	return res, nil
}

func (o *openaiProv) Models(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, o.hc, o.base()+"/models", o.headers(), &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}
