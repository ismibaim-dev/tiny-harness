package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Test-only provider ("type": "mock"): scripted replies, streamed, no network.
type mockProv struct{}

func (mockProv) Models(ctx context.Context) ([]string, error) {
	return []string{"mock-1", "mock-2"}, nil
}

func (mockProv) Complete(ctx context.Context, r Request) (Response, error) {
	res := Response{Usage: Usage{In: 100 * len(r.Msgs), Out: 20}}
	say := func(s string) Response {
		for _, w := range strings.SplitAfter(s, " ") {
			if r.OnText != nil {
				r.OnText(w)
			}
			time.Sleep(5 * time.Millisecond)
		}
		res.Blocks = append(res.Blocks, Block{Type: "text", Text: s})
		return res
	}
	if strings.Contains(r.System, "summariz") {
		return say("SUMMARY of earlier work (" + r.Model + ")."), nil
	}
	last := r.Msgs[len(r.Msgs)-1]
	if last.Role == "user" && len(last.Content) > 0 && last.Content[0].Type == "tool_result" {
		return say("Mock: tool finished. Output:\n```\n" + trunc(last.Content[0].Content, 200) + "\n```"), nil
	}
	text, imgs := imageBlocks(last)
	t := strings.Join(text, " ")
	use := func(name string, in map[string]any) Response {
		raw, _ := json.Marshal(in)
		say("Mock: calling " + name)
		res.Blocks = append(res.Blocks, Block{Type: "tool_use", ID: "mock_" + randHex(4), Name: name, Input: raw})
		return res
	}
	switch {
	case strings.HasPrefix(t, "run "):
		return use("bash", map[string]any{"command": strings.TrimPrefix(t, "run ")}), nil
	case strings.HasPrefix(t, "list"):
		return use("list_dir", map[string]any{"path": "."}), nil
	case strings.HasPrefix(t, "write"):
		return use("write_file", map[string]any{"path": "hello.txt", "content": "hi from mock\n"}), nil
	case strings.HasPrefix(t, "tool "):
		f := strings.Fields(t)
		args := map[string]any{}
		if len(f) > 2 {
			_ = json.Unmarshal([]byte(strings.Join(f[2:], " ")), &args)
		}
		return use(f[1], args), nil
	case strings.HasPrefix(t, "slow"):
		select {
		case <-time.After(30 * time.Second):
		case <-ctx.Done():
			return res, ctx.Err()
		}
	}
	return say(fmt.Sprintf("Mock reply (%s, %d image(s), %d msgs): %s", r.Model, len(imgs), len(r.Msgs), t)), nil
}
