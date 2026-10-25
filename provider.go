package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"
)

// Message and Block are the internal (and persisted) conversation format.
// Each provider adapter converts them to its own wire format.
type Message struct {
	Role    string  `json:"role"` // "user" | "assistant"
	Content []Block `json:"content"`
}

type Block struct {
	Type      string          `json:"type"` // text | image | tool_use | tool_result | reasoning
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Media     string          `json:"media,omitempty"` // image mime type
	Data      string          `json:"data,omitempty"`  // image base64
	// Opaque provider state that must be echoed back to the same provider/model:
	// Gemini thoughtSignature, OpenAI-compat extra_content, reasoning field name.
	Sig string `json:"sig,omitempty"`
	Src string `json:"src,omitempty"` // "profile/model" that produced Sig or reasoning
}

type ToolDef struct {
	Name        string
	Description string
	Schema      map[string]any
}

type Request struct {
	System string
	Msgs   []Message
	Tools  []ToolDef
	Model  string
	OnText func(delta string) // streamed visible text
}

type Usage struct {
	In        int `json:"in"`  // prompt tokens (including cached)
	Out       int `json:"out"` // completion tokens
	CacheRead int `json:"cache_read,omitempty"`
}

type Response struct {
	Blocks []Block
	Usage  Usage
	Stop   string // "max_tokens" when the reply was cut off
}

type Provider interface {
	Complete(ctx context.Context, r Request) (Response, error)
	Models(ctx context.Context) ([]string, error)
}

// ------------------------------------------------------------------ HTTP

func newHTTPClient() *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.ResponseHeaderTimeout = 3 * time.Minute
	tr.MaxIdleConnsPerHost = 4
	tr.IdleConnTimeout = 90 * time.Second
	return &http.Client{Transport: tr} // no overall timeout: streams can be long; ctx bounds them
}

type httpError struct {
	Status int
	Body   string
}

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Status, e.Body) }

// send does a request with retries on 429/5xx/network errors (only before any
// body has been consumed) and returns a 200 response for the caller to read.
func send(ctx context.Context, hc *http.Client, method, url string, headers map[string]string, body any) (*http.Response, error) {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			wait := time.Duration(1<<attempt) * time.Second
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		var rd io.Reader
		if payload != nil {
			rd = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rd)
		if err != nil {
			return nil, err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			lastErr = err
			continue
		}
		if resp.StatusCode == 200 {
			return resp, nil
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		lastErr = &httpError{resp.StatusCode, trunc(strings.TrimSpace(string(data)), 800)}
		if resp.StatusCode != 429 && resp.StatusCode < 500 {
			return nil, lastErr
		}
	}
	return nil, lastErr
}

func getJSON(ctx context.Context, hc *http.Client, url string, headers map[string]string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	resp, err := send(ctx, hc, "GET", url, headers, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

var errStalled = errors.New("stream stalled (no data for 2 minutes)")

// readSSE parses a server-sent-events stream and calls fn per event.
// It cancels the request if no bytes arrive for `idle`.
func readSSE(ctx context.Context, cancel context.CancelFunc, body io.Reader, fn func(event, data string) error) error {
	const idle = 2 * time.Minute
	var stalled atomic.Bool
	t := time.AfterFunc(idle, func() { stalled.Store(true); cancel() })
	defer t.Stop()
	br := bufio.NewReaderSize(io.LimitReader(body, 256<<20), 64<<10)
	var event string
	var data []string
	flush := func() error {
		if len(data) == 0 {
			event = ""
			return nil
		}
		d := strings.Join(data, "\n")
		ev := event
		event, data = "", data[:0]
		return fn(ev, d)
	}
	for {
		line, err := br.ReadString('\n')
		t.Reset(idle)
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "":
			if ferr := flush(); ferr != nil {
				return ferr
			}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[6:])
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(line[5:], " "))
		}
		if err != nil {
			if ferr := flush(); ferr != nil {
				return ferr
			}
			if err == io.EOF {
				return nil
			}
			if stalled.Load() {
				return errStalled
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
	}
}

// merge copies a profile's "extra" body fields into a request body.
func merge(body map[string]any, extra map[string]any) {
	for k, v := range extra {
		body[k] = v
	}
}

func cleanBlocks(bs []Block) []Block {
	out := bs[:0]
	for _, b := range bs {
		if b.Type == "tool_use" && (len(b.Input) == 0 || !json.Valid(b.Input)) {
			b.Input = json.RawMessage(`{}`)
		}
		out = append(out, b)
	}
	return out
}

func imageBlocks(m Message) (text []string, imgs []Block) {
	for _, b := range m.Content {
		switch b.Type {
		case "text":
			text = append(text, b.Text)
		case "image":
			imgs = append(imgs, b)
		}
	}
	return
}
