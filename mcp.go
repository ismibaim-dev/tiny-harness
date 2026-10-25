package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Minimal MCP client over stdio (newline-delimited JSON-RPC 2.0).
// Supports initialize, tools/list (with pagination) and tools/call.

type MCPConfig struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Dir         string            `json:"dir,omitempty"`
	AutoApprove []string          `json:"auto_approve,omitempty"` // tool names, or "*"
	TimeoutSec  int               `json:"timeout_sec,omitempty"`
}

type mcpClient struct {
	name    string
	cfg     MCPConfig
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	wmu     sync.Mutex
	nextID  atomic.Int64
	pmu     sync.Mutex
	pending map[int64]chan rpcResp
	dead    chan struct{}
}

type rpcResp struct {
	Result json.RawMessage
	Err    error
}

var mcpNameRe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

func startMCP(ctx context.Context, name string, cfg MCPConfig) (*mcpClient, error) {
	cmd := exec.CommandContext(ctx, cfg.Command, cfg.Args...)
	cmd.Env = os.Environ()
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, k+"="+os.ExpandEnv(v))
	}
	cmd.Dir = cfg.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	c := &mcpClient{name: name, cfg: cfg, cmd: cmd, stdin: stdin,
		pending: map[int64]chan rpcResp{}, dead: make(chan struct{})}
	go c.readLoop(stdout)
	go func() { _ = cmd.Wait(); close(c.dead) }()
	return c, nil
}

func (c *mcpClient) write(v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err = c.stdin.Write(append(raw, '\n'))
	return err
}

func (c *mcpClient) readLoop(r io.Reader) {
	br := bufio.NewReaderSize(r, 64<<10)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			c.handle(line)
		}
		if err != nil {
			break
		}
	}
	c.pmu.Lock()
	for id, ch := range c.pending {
		ch <- rpcResp{Err: errors.New("MCP server exited")}
		delete(c.pending, id)
	}
	c.pmu.Unlock()
}

func (c *mcpClient) handle(line []byte) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &m) != nil {
		return // servers sometimes log to stdout; ignore non-JSON
	}
	if m.Method != "" {
		if len(m.ID) == 0 || string(m.ID) == "null" {
			return // notification
		}
		if m.Method == "ping" {
			_ = c.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": map[string]any{}})
			return
		}
		_ = c.write(map[string]any{"jsonrpc": "2.0", "id": m.ID,
			"error": map[string]any{"code": -32601, "message": "method not supported by tiny-harness"}})
		return
	}
	var id int64
	if json.Unmarshal(m.ID, &id) != nil {
		return
	}
	c.pmu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.pmu.Unlock()
	if ch == nil {
		return
	}
	if m.Error != nil {
		ch <- rpcResp{Err: fmt.Errorf("MCP error %d: %s", m.Error.Code, m.Error.Message)}
		return
	}
	ch <- rpcResp{Result: m.Result}
}

func (c *mcpClient) call(ctx context.Context, method string, params any, timeout time.Duration) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcResp, 1)
	c.pmu.Lock()
	c.pending[id] = ch
	c.pmu.Unlock()
	cleanup := func() { c.pmu.Lock(); delete(c.pending, id); c.pmu.Unlock() }
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		cleanup()
		return nil, err
	}
	select {
	case r := <-ch:
		return r.Result, r.Err
	case <-ctx.Done():
		cleanup()
		_ = c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
			"params": map[string]any{"requestId": id, "reason": "cancelled by user"}})
		return nil, ctx.Err()
	case <-time.After(timeout):
		cleanup()
		return nil, fmt.Errorf("MCP %s timed out after %s", method, timeout)
	case <-c.dead:
		cleanup()
		return nil, errors.New("MCP server exited")
	}
}

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func (c *mcpClient) init(ctx context.Context) ([]mcpTool, error) {
	_, err := c.call(ctx, "initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "tiny-harness", "version": version},
	}, 30*time.Second)
	if err != nil {
		return nil, err
	}
	if err := c.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}); err != nil {
		return nil, err
	}
	var all []mcpTool
	cursor := ""
	for page := 0; page < 20; page++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.call(ctx, "tools/list", params, 30*time.Second)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []mcpTool `json:"tools"`
			NextCursor string    `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, err
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return all, nil
}

func (c *mcpClient) toolFor(t mcpTool) *Tool {
	full := "mcp__" + mcpNameRe.ReplaceAllString(c.name, "_") + "__" + mcpNameRe.ReplaceAllString(t.Name, "_")
	if len(full) > 64 {
		full = full[:64]
	}
	kind := "exec"
	for _, a := range c.cfg.AutoApprove {
		if a == "*" || a == t.Name {
			kind = "read"
		}
	}
	sch := t.InputSchema
	if sch == nil {
		sch = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	delete(sch, "$schema")
	timeout := time.Duration(c.cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	desc := t.Description
	if desc == "" {
		desc = t.Name
	}
	return &Tool{
		Name: full, Kind: kind, Schema: sch,
		Desc: "[MCP " + c.name + "] " + trunc(desc, 1500),
		Run: func(ctx context.Context, e *Env, in json.RawMessage) (string, error) {
			var args any = map[string]any{}
			if len(in) > 0 {
				_ = json.Unmarshal(in, &args)
			}
			raw, err := c.call(ctx, "tools/call", map[string]any{"name": t.Name, "arguments": args}, timeout)
			if err != nil {
				return "", err
			}
			var res struct {
				Content []struct {
					Type     string `json:"type"`
					Text     string `json:"text"`
					MimeType string `json:"mimeType"`
					Resource struct {
						URI  string `json:"uri"`
						Text string `json:"text"`
					} `json:"resource"`
				} `json:"content"`
				Structured json.RawMessage `json:"structuredContent"`
				IsError    bool            `json:"isError"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				return "", err
			}
			var b strings.Builder
			for _, ct := range res.Content {
				switch ct.Type {
				case "text":
					b.WriteString(ct.Text)
				case "resource":
					b.WriteString(ct.Resource.URI + "\n" + ct.Resource.Text)
				default:
					fmt.Fprintf(&b, "[%s content (%s) not shown]", ct.Type, ct.MimeType)
				}
				b.WriteString("\n")
			}
			if b.Len() == 0 && len(res.Structured) > 0 {
				b.Write(res.Structured)
			}
			out := trunc(b.String(), 60000)
			if res.IsError {
				return "", errors.New(out)
			}
			return out, nil
		},
	}
}

// startAllMCP launches configured servers and registers their tools.
func startAllMCP(ctx context.Context, servers map[string]MCPConfig) (count int) {
	for name, cfg := range servers {
		c, err := startMCP(ctx, name, cfg)
		if err != nil {
			log.Printf("mcp %s: start failed: %v", name, err)
			continue
		}
		ts, err := c.init(ctx)
		if err != nil {
			log.Printf("mcp %s: init failed: %v", name, err)
			_ = c.cmd.Process.Kill()
			continue
		}
		for _, t := range ts {
			registerTool(c.toolFor(t))
			count++
		}
		log.Printf("mcp %s: %d tools", name, len(ts))
	}
	return count
}
