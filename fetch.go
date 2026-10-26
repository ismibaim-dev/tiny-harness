package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
)

var (
	reDrop  = regexp.MustCompile(`(?is)<(script|style|noscript|svg|head|template)\b.*?</(script|style|noscript|svg|head|template)>`)
	reBlock = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/tr|/h[1-6]|/pre|/blockquote|hr)\b[^>]*>`)
	reTag   = regexp.MustCompile(`(?s)<[^>]*>`)
	reBlank = regexp.MustCompile(`[ \t\r\f\v]+`)
	reLines = regexp.MustCompile(`\n\s*\n+`)
)

// htmlToText is a deliberately crude extractor: good enough for docs and
// articles, and costs nothing compared with a real HTML parser.
func htmlToText(s string) string {
	s = reDrop.ReplaceAllString(s, " ")
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, " ")
	s = html.UnescapeString(s)
	s = reBlank.ReplaceAllString(s, " ")
	s = reLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

func toolFetch() *Tool {
	return &Tool{
		Name: "web_fetch", Kind: "net",
		Desc: "Fetch a public http(s) URL and return its text (HTML is converted to plain text). " +
			"Use offset to read further into long pages.",
		Schema: schema(map[string]any{
			"url":    sType("string", "The URL to fetch."),
			"offset": sType("integer", "Character offset to start from (default 0)."),
		}, "url"),
		Summary: func(in json.RawMessage) string { return argStr(in, "url") },
		Run: func(ctx context.Context, e *Env, in json.RawMessage) (string, error) {
			var a struct {
				URL    string `json:"url"`
				Offset int    `json:"offset"`
			}
			if err := json.Unmarshal(in, &a); err != nil {
				return "", err
			}
			if !strings.HasPrefix(a.URL, "http://") && !strings.HasPrefix(a.URL, "https://") {
				return "", errors.New("only http(s) URLs are supported")
			}
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, "GET", a.URL, nil)
			if err != nil {
				return "", err
			}
			req.Header.Set("User-Agent", "tiny-harness/0.2 (+agent web_fetch)")
			req.Header.Set("Accept", "text/html,text/plain,application/json;q=0.9,*/*;q=0.5")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
			if err != nil {
				return "", err
			}
			ct := resp.Header.Get("Content-Type")
			body := strings.ToValidUTF8(string(raw), "")
			if strings.Contains(ct, "html") || (ct == "" && strings.Contains(strings.ToLower(body[:min(len(body), 512)]), "<html")) {
				body = htmlToText(body)
			} else if !strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "json") && !strings.Contains(ct, "xml") && ct != "" {
				return fmt.Sprintf("HTTP %d, %s, %d bytes (binary content not shown)", resp.StatusCode, ct, len(raw)), nil
			}
			if a.Offset > 0 {
				if a.Offset >= len(body) {
					return fmt.Sprintf("(offset beyond end; %d characters total)", len(body)), nil
				}
				body = body[a.Offset:]
			}
			page := outputCap(e.Cfg) - 300
			head := fmt.Sprintf("HTTP %d  %s\n\n", resp.StatusCode, ct)
			if len(body) > page {
				return head + strings.ToValidUTF8(body[:page], "") +
					fmt.Sprintf("\n[truncated; continue with offset=%d]", a.Offset+page), nil
			}
			return head + body, nil
		},
	}
}
