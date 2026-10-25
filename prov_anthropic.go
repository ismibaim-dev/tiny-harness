package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type anthropicProv struct {
	name string
	p    Profile
	key  string
	hc   *http.Client
}

func (a *anthropicProv) base() string {
	if b := strings.TrimRight(a.p.BaseURL, "/"); b != "" {
		return b
	}
	return "https://api.anthropic.com"
}

func (a *anthropicProv) headers() map[string]string {
	return map[string]string{"x-api-key": a.key, "anthropic-version": "2023-06-01"}
}

var ephemeral = map[string]any{"type": "ephemeral"}

// toAnthropic converts history; the last block of the last message gets a
// cache breakpoint so each step re-reads the conversation prefix from cache.
func toAnthropic(msgs []Message) []map[string]any {
	out := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		var content []map[string]any
		for _, b := range m.Content {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					content = append(content, map[string]any{"type": "text", "text": b.Text})
				}
			case "image":
				content = append(content, map[string]any{"type": "image",
					"source": map[string]any{"type": "base64", "media_type": b.Media, "data": b.Data}})
			case "tool_use":
				in := b.Input
				if len(in) == 0 {
					in = json.RawMessage(`{}`)
				}
				content = append(content, map[string]any{"type": "tool_use", "id": b.ID, "name": b.Name, "input": in})
			case "tool_result":
				c := map[string]any{"type": "tool_result", "tool_use_id": b.ToolUseID, "content": b.Content}
				if b.IsError {
					c["is_error"] = true
				}
				content = append(content, c)
			}
		}
		if len(content) == 0 {
			content = []map[string]any{{"type": "text", "text": "(empty)"}}
		}
		out = append(out, map[string]any{"role": m.Role, "content": content})
	}
	if n := len(out); n > 0 {
		c := out[n-1]["content"].([]map[string]any)
		c[len(c)-1]["cache_control"] = ephemeral
	}
	return out
}

func (a *anthropicProv) Complete(ctx context.Context, r Request) (Response, error) {
	maxTok := a.p.MaxTokens
	if maxTok <= 0 {
		maxTok = 16000
	}
	tdefs := make([]map[string]any, 0, len(r.Tools))
	for _, t := range r.Tools {
		tdefs = append(tdefs, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.Schema})
	}
	body := map[string]any{
		"model":      r.Model,
		"max_tokens": maxTok,
		"stream":     true,
		"system":     []map[string]any{{"type": "text", "text": r.System, "cache_control": ephemeral}},
		"messages":   toAnthropic(r.Msgs),
	}
	if len(tdefs) > 0 {
		body["tools"] = tdefs
	}
	merge(body, a.p.Extra)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	resp, err := send(ctx, a.hc, "POST", a.base()+"/v1/messages", a.headers(), body)
	if err != nil {
		return Response{}, err
	}
	defer resp.Body.Close()

	type acc struct {
		b    Block
		text strings.Builder
		js   strings.Builder
	}
	var blocks []*acc
	byIndex := map[int]*acc{}
	var res Response
	type usage struct {
		In          int `json:"input_tokens"`
		Out         int `json:"output_tokens"`
		CacheRead   int `json:"cache_read_input_tokens"`
		CacheCreate int `json:"cache_creation_input_tokens"`
	}
	err = readSSE(ctx, cancel, resp.Body, func(_, data string) error {
		var e struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Message struct {
				Usage usage `json:"usage"`
			} `json:"message"`
			Usage usage `json:"usage"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(data), &e) != nil {
			return nil
		}
		switch e.Type {
		case "message_start":
			u := e.Message.Usage
			res.Usage.In = u.In + u.CacheRead + u.CacheCreate
			res.Usage.CacheRead = u.CacheRead
			res.Usage.Out = u.Out
		case "content_block_start":
			cb := &acc{b: Block{Type: e.ContentBlock.Type, ID: e.ContentBlock.ID, Name: e.ContentBlock.Name}}
			cb.text.WriteString(e.ContentBlock.Text)
			byIndex[e.Index] = cb
			blocks = append(blocks, cb)
		case "content_block_delta":
			cb := byIndex[e.Index]
			if cb == nil {
				return nil
			}
			switch e.Delta.Type {
			case "text_delta":
				cb.text.WriteString(e.Delta.Text)
				if r.OnText != nil {
					r.OnText(e.Delta.Text)
				}
			case "input_json_delta":
				cb.js.WriteString(e.Delta.PartialJSON)
			}
		case "message_delta":
			if e.Usage.Out > 0 {
				res.Usage.Out = e.Usage.Out
			}
			if e.Delta.StopReason == "max_tokens" {
				res.Stop = "max_tokens"
			}
		case "error":
			return fmt.Errorf("%s: %s", e.Error.Type, e.Error.Message)
		}
		return nil
	})
	if err != nil {
		return res, err
	}
	for _, cb := range blocks {
		switch cb.b.Type {
		case "text":
			if t := cb.text.String(); strings.TrimSpace(t) != "" {
				res.Blocks = append(res.Blocks, Block{Type: "text", Text: t})
			}
		case "tool_use":
			cb.b.Input = json.RawMessage(cb.js.String())
			res.Blocks = append(res.Blocks, cb.b)
		}
	}
	res.Blocks = cleanBlocks(res.Blocks)
	return res, nil
}

func (a *anthropicProv) Models(ctx context.Context) ([]string, error) {
	var out struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, a.hc, a.base()+"/v1/models?limit=100", a.headers(), &out); err != nil {
		return nil, err
	}
	var ids []string
	for _, d := range out.Data {
		ids = append(ids, d.ID)
	}
	return ids, nil
}
