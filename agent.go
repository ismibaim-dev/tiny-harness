package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Event is one line in the session timeline the phone UI renders.
type Event struct {
	N    int    `json:"n"`
	Type string `json:"type"` // user assistant tool_call tool_result approval approval_done error status compact
	ID   string `json:"id,omitempty"`
	Name string `json:"name,omitempty"`
	Text string `json:"text,omitempty"`
	Err  bool   `json:"err,omitempty"`
	Imgs int    `json:"imgs,omitempty"`
}

type Session struct {
	ID  string
	srv *Server

	mu        sync.Mutex
	title     string
	Profile   string
	Model     string // "" = profile default
	Mode      string // ask | auto-edit | yolo
	Workspace string
	msgs      []Message
	events    []Event
	notify    chan struct{}
	epoch     string
	busy      bool
	cancel    context.CancelFunc
	pending   map[string]chan bool
	turnAllow bool
	lastUsed  time.Time
	live      string // text being streamed right now
	liveVer   int
	usage     Usage
	lastIn    int    // prompt size of the latest call (≈ context used)
	started   string // date in the system prompt, fixed per chat so prompt caches keep hitting
}

type stored struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Profile   string    `json:"profile"`
	Model     string    `json:"model,omitempty"`
	Mode      string    `json:"mode"`
	Workspace string    `json:"workspace,omitempty"`
	Updated   int64     `json:"updated"`
	Usage     Usage     `json:"usage"`
	LastIn    int       `json:"last_in,omitempty"`
	Started   string    `json:"started,omitempty"`
	Msgs      []Message `json:"msgs,omitempty"`
}

const summaryHeader = "[Summary of the conversation so far]"

var errBusy = errors.New("the agent is still working on the previous message")

func newSessionObj(srv *Server, id, profile, mode, ws string) *Session {
	return &Session{
		ID: id, srv: srv, Profile: profile, Mode: mode, Workspace: ws,
		notify: make(chan struct{}), epoch: randHex(4),
		pending: map[string]chan bool{}, lastUsed: time.Now(),
	}
}

func (s *Session) touch() {
	s.mu.Lock()
	s.lastUsed = time.Now()
	s.mu.Unlock()
}

// wake must be called with s.mu held.
func (s *Session) wake() {
	close(s.notify)
	s.notify = make(chan struct{})
}

func (s *Session) emit(e Event) {
	s.mu.Lock()
	e.N = len(s.events) + 1
	s.events = append(s.events, e)
	s.wake()
	s.mu.Unlock()
}

func (s *Session) liveAppend(delta string) {
	s.mu.Lock()
	s.live += delta
	s.liveVer++
	s.wake()
	s.mu.Unlock()
}

// finishLive replaces the streaming bubble with final events atomically.
func (s *Session) finishLive(evs ...Event) {
	s.mu.Lock()
	for _, e := range evs {
		e.N = len(s.events) + 1
		s.events = append(s.events, e)
	}
	if s.live != "" {
		s.live = ""
		s.liveVer++
	}
	s.wake()
	s.mu.Unlock()
}

type pollResult struct {
	Events    []Event `json:"events"`
	Next      int     `json:"next"`
	Epoch     string  `json:"epoch"`
	Reset     bool    `json:"reset,omitempty"`
	Busy      bool    `json:"busy"`
	Title     string  `json:"title"`
	Profile   string  `json:"profile"`
	Model     string  `json:"model"`
	Mode      string  `json:"mode"`
	Workspace string  `json:"workspace"`
	Live      string  `json:"live"`
	LV        int     `json:"lv"`
	Usage     Usage   `json:"usage"`
	Ctx       int     `json:"ctx"`
}

// snapshot returns what changed after (since, lv) plus a channel closed on the next change.
func (s *Session) snapshot(since int, epoch string, lv int) (pollResult, bool, <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := pollResult{Epoch: s.epoch, Busy: s.busy, Title: s.title, Profile: s.Profile, Model: s.modelLocked(),
		Mode: s.Mode, Workspace: s.Workspace, Live: s.live, LV: s.liveVer, Usage: s.usage, Ctx: s.lastIn,
		Events: []Event{}}
	if epoch != s.epoch || since < 0 || since > len(s.events) {
		since = 0
		r.Reset = true
	}
	r.Events = append(r.Events, s.events[since:]...)
	r.Next = len(s.events)
	changed := r.Reset || len(r.Events) > 0 || lv != s.liveVer
	return r, changed, s.notify
}

func (s *Session) modelLocked() string {
	if s.Model != "" {
		return s.Model
	}
	return s.srv.cfg.Profiles[s.Profile].Model
}

// Send starts a turn. Images are base64 blocks the phone already resized.
func (s *Session) Send(text string, images []Block) error {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		return errBusy
	}
	s.busy = true
	s.lastUsed = time.Now()
	if s.title == "" {
		s.title = oneLine(trunc(text, 60))
	}
	blocks := append([]Block{{Type: "text", Text: text}}, images...)
	if n := len(s.msgs); n > 0 && s.msgs[n-1].Role == "user" {
		// previous turn failed before the model answered: merge, keep roles alternating
		s.msgs[n-1].Content = append(s.msgs[n-1].Content, blocks...)
	} else {
		s.msgs = append(s.msgs, Message{Role: "user", Content: blocks})
	}
	s.mu.Unlock()
	s.emit(Event{Type: "user", Text: text, Imgs: len(images)})
	go s.runTurn()
	return nil
}

func (s *Session) Stop() {
	s.mu.Lock()
	c := s.cancel
	s.mu.Unlock()
	if c != nil {
		c()
	}
}

func (s *Session) Approve(callID string, allow, all bool) error {
	s.mu.Lock()
	ch := s.pending[callID]
	if ch != nil && allow && all {
		s.turnAllow = true
	}
	s.mu.Unlock()
	if ch == nil {
		return errors.New("no such pending approval")
	}
	select {
	case ch <- allow:
	default:
	}
	return nil
}

func (s *Session) Title() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title
}

// begin marks the session busy for background work (turns, manual compaction).
func (s *Session) begin() (context.Context, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.busy, s.cancel, s.turnAllow = true, cancel, false
	return ctx, true
}

func (s *Session) end() {
	s.mu.Lock()
	if s.cancel != nil {
		s.cancel()
	}
	s.busy, s.cancel, s.turnAllow = false, nil, false
	s.lastUsed = time.Now()
	s.mu.Unlock()
	s.emit(Event{Type: "status", Text: "idle"})
	s.save()
}

// ------------------------------------------------------------------ the loop

func (s *Session) runTurn() {
	start := time.Now()
	// Send already set busy under the lock; attach the cancel func.
	ctx, cancel := context.WithCancel(context.Background())
	s.mu.Lock()
	s.cancel, s.turnAllow = cancel, false
	s.mu.Unlock()
	s.emit(Event{Type: "status", Text: "busy"})
	defer func() {
		if r := recover(); r != nil {
			s.finishLive(Event{Type: "error", Text: fmt.Sprint("internal error: ", r), Err: true})
		}
		s.end()
		if time.Since(start) > 20*time.Second {
			s.srv.ping("Task finished", s.Title())
		}
	}()

	s.mu.Lock()
	profile, model, ws := s.Profile, s.modelLocked(), s.Workspace
	if s.started == "" {
		s.started = time.Now().Format("Monday 2 January 2006")
	}
	started := s.started
	s.mu.Unlock()
	prov, err := s.srv.provider(profile)
	if err != nil {
		s.emit(Event{Type: "error", Text: err.Error(), Err: true})
		return
	}
	env := s.srv.envFor(ws)
	system := s.srv.systemPrompt(env, started)
	defs := toolDefs()
	cprov, cmodel := s.srv.compactWith(prov, model)

	for step := 0; step < s.srv.cfg.MaxSteps; step++ {
		if ctx.Err() != nil {
			s.emit(Event{Type: "error", Text: "stopped", Err: true})
			return
		}
		if s.prepare() > s.srv.cfg.MaxContextChars {
			if err := s.compact(ctx, cprov, cmodel, defs, false); err != nil && ctx.Err() == nil {
				s.emit(Event{Type: "error", Text: "auto-compaction skipped: " + err.Error(), Err: true})
			}
		}
		msgs := s.snapshotMsgs()
		cctx, ccancel := context.WithTimeout(ctx, 15*time.Minute)
		res, err := prov.Complete(cctx, Request{System: system, Msgs: msgs, Tools: defs, Model: model, OnText: s.liveAppend})
		ccancel()
		s.addUsage(res.Usage)
		if err != nil {
			var evs []Event
			s.mu.Lock()
			partial := s.live
			s.mu.Unlock()
			if strings.TrimSpace(partial) != "" {
				evs = append(evs, Event{Type: "assistant", Text: partial + " …"})
			}
			msg := "model error: " + err.Error()
			if ctx.Err() != nil {
				msg = "stopped"
			}
			s.finishLive(append(evs, Event{Type: "error", Text: msg, Err: true})...)
			return
		}
		if len(res.Blocks) == 0 {
			res.Blocks = []Block{{Type: "text", Text: "(no response)"}}
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, Message{Role: "assistant", Content: res.Blocks})
		s.mu.Unlock()

		var evs []Event
		var uses []Block
		for _, b := range res.Blocks {
			switch {
			case b.Type == "text" && strings.TrimSpace(b.Text) != "":
				evs = append(evs, Event{Type: "assistant", Text: b.Text})
			case b.Type == "tool_use":
				uses = append(uses, b)
			}
		}
		if res.Stop == "max_tokens" {
			evs = append(evs, Event{Type: "error", Text: "reply hit the max_tokens limit (raise max_tokens in the profile)", Err: true})
		}
		s.finishLive(evs...)
		if len(uses) == 0 {
			return
		}
		results := make([]Block, 0, len(uses))
		for _, u := range uses {
			results = append(results, s.execTool(ctx, env, u))
		}
		s.mu.Lock()
		s.msgs = append(s.msgs, Message{Role: "user", Content: results})
		s.mu.Unlock()
		if step%5 == 4 {
			s.save()
		}
	}
	s.emit(Event{Type: "error", Text: fmt.Sprintf("stopped after %d steps; send a message to continue", s.srv.cfg.MaxSteps), Err: true})
}

func (s *Session) addUsage(u Usage) {
	s.mu.Lock()
	s.usage.In += u.In
	s.usage.Out += u.Out
	s.usage.CacheRead += u.CacheRead
	if u.In > 0 {
		s.lastIn = u.In
	}
	s.mu.Unlock()
}

func (s *Session) execTool(ctx context.Context, env *Env, u Block) Block {
	res := Block{Type: "tool_result", ToolUseID: u.ID}
	finish := func(content string, isErr bool) Block {
		if content == "" {
			content = "(no output)"
		}
		content = clipOutput(content, outputCap(s.srv.cfg))
		res.Content, res.IsError = content, isErr
		s.emit(Event{Type: "tool_result", ID: u.ID, Text: trunc(content, 3000), Err: isErr})
		return res
	}
	t, ok := toolByName[u.Name]
	if !ok {
		s.emit(Event{Type: "tool_call", ID: u.ID, Name: u.Name, Text: "(unknown tool)"})
		return finish("unknown tool: "+u.Name, true)
	}
	summary := t.Summary(u.Input)
	s.emit(Event{Type: "tool_call", ID: u.ID, Name: u.Name, Text: summary})
	if ctx.Err() != nil {
		return finish("cancelled by user", true)
	}
	if s.needsApproval(t, u.Input) && !s.askApproval(ctx, u, summary) {
		return finish("the user denied this action", true)
	}
	out, err := t.Run(ctx, env, u.Input)
	if err != nil {
		return finish("error: "+err.Error(), true)
	}
	return finish(out, false)
}

func (s *Session) needsApproval(t *Tool, in json.RawMessage) bool {
	if t.Kind == "read" {
		return false
	}
	s.mu.Lock()
	mode, all := s.Mode, s.turnAllow
	s.mu.Unlock()
	if mode == "yolo" || all {
		return false
	}
	if t.Name == "bash" && s.srv.isSafeCommand(argStr(in, "command")) {
		return false
	}
	if mode == "auto-edit" && (t.Kind == "edit" || t.Kind == "net") {
		return false
	}
	return true
}

func (s *Session) askApproval(ctx context.Context, u Block, detail string) bool {
	ch := make(chan bool, 1)
	s.mu.Lock()
	s.pending[u.ID] = ch
	s.mu.Unlock()
	s.emit(Event{Type: "approval", ID: u.ID, Name: u.Name, Text: detail})
	s.srv.ping("Approval needed: "+u.Name, firstLine(detail))
	allowed := false
	select {
	case allowed = <-ch:
	case <-ctx.Done():
	case <-time.After(30 * time.Minute):
	}
	s.mu.Lock()
	delete(s.pending, u.ID)
	s.mu.Unlock()
	verdict := "denied"
	if allowed {
		verdict = "allowed"
	}
	s.emit(Event{Type: "approval_done", ID: u.ID, Text: verdict})
	return allowed
}

// ------------------------------------------------------------ context mgmt

func blockCost(b Block) int {
	if b.Type == "image" {
		return 6000 // ≈1.5k tokens, not the base64 length
	}
	return len(b.Text) + len(b.Content) + len(b.Input) + 20
}

// prepare repairs the history and elides old bulky content when it grows past
// the budget. Returns the remaining size in characters.
func (s *Session) prepare() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.msgs = repairHistory(s.msgs)
	total := 0
	for _, m := range s.msgs {
		for _, b := range m.Content {
			total += blockCost(b)
		}
	}
	limit := s.srv.cfg.MaxContextChars
	if total <= limit {
		return total
	}
	keep := len(s.msgs) - 6
	for i := 0; i < keep && total > limit*8/10; i++ {
		for j := range s.msgs[i].Content {
			b := &s.msgs[i].Content[j]
			switch {
			case b.Type == "tool_result" && len(b.Content) > 300:
				total -= blockCost(*b)
				b.Content = trunc(b.Content, 200) + "\n[older output elided to save context]"
				total += blockCost(*b)
			case b.Type == "image":
				total -= blockCost(*b)
				*b = Block{Type: "text", Text: "[image removed to save context]"}
				total += blockCost(*b)
			case b.Type == "reasoning" && len(b.Text) > 0:
				total -= blockCost(*b)
				b.Text = ""
			}
		}
	}
	return total
}

// repairHistory gives every tool_use a tool_result (a crash or restart can leave one dangling).
func repairHistory(msgs []Message) []Message {
	for i := 0; i < len(msgs); i++ {
		if msgs[i].Role != "assistant" {
			continue
		}
		var missing []Block
		have := map[string]bool{}
		if i+1 < len(msgs) && msgs[i+1].Role == "user" {
			for _, b := range msgs[i+1].Content {
				if b.Type == "tool_result" {
					have[b.ToolUseID] = true
				}
			}
		}
		for _, b := range msgs[i].Content {
			if b.Type == "tool_use" && !have[b.ID] {
				missing = append(missing, Block{Type: "tool_result", ToolUseID: b.ID, Content: "(interrupted; no result)", IsError: true})
			}
		}
		if len(missing) == 0 {
			continue
		}
		if i+1 < len(msgs) && msgs[i+1].Role == "user" {
			msgs[i+1].Content = append(missing, msgs[i+1].Content...)
		} else {
			msgs = append(msgs[:i+1], append([]Message{{Role: "user", Content: missing}}, msgs[i+1:]...)...)
		}
	}
	return msgs
}

func (s *Session) snapshotMsgs() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Message, len(s.msgs))
	for i, m := range s.msgs {
		out[i] = Message{Role: m.Role, Content: append([]Block(nil), m.Content...)}
	}
	return out
}

func hasText(m Message) bool {
	for _, b := range m.Content {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return true
		}
	}
	return false
}

const compactPrompt = `Summarize the conversation above so the work can continue in a fresh context. Include: the user's goals and constraints, decisions made, files created or changed (with paths), commands that mattered and their outcome, errors still open, and what remains to do. Be concise and factual; use short bullet points. Do not call any tools.`

// compact replaces older history with a model-written summary. With all=false
// it keeps the current turn (from the latest user text message) intact.
func (s *Session) compact(ctx context.Context, prov Provider, model string, defs []ToolDef, all bool) error {
	msgs := s.snapshotMsgs()
	idx := len(msgs)
	if !all {
		idx = -1
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "user" && hasText(msgs[i]) {
				idx = i
				break
			}
		}
	}
	if idx < 2 {
		return errors.New("not enough history before the current request to summarize")
	}
	head := append([]Message(nil), msgs[:idx]...)
	if head[len(head)-1].Role == "user" {
		head[len(head)-1].Content = append(head[len(head)-1].Content, Block{Type: "text", Text: compactPrompt})
	} else {
		head = append(head, Message{Role: "user", Content: []Block{{Type: "text", Text: compactPrompt}}})
	}
	s.emit(Event{Type: "status", Text: "compacting"})
	cctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	res, err := prov.Complete(cctx, Request{
		System: "You are summarizing a coding-agent session for a handover. Reply with the summary text only.",
		Msgs:   head, Tools: defs, Model: model,
	})
	s.addUsage(res.Usage)
	if err != nil {
		return err
	}
	var sum []string
	for _, b := range res.Blocks {
		if b.Type == "text" {
			sum = append(sum, b.Text)
		}
	}
	summary := strings.TrimSpace(strings.Join(sum, "\n"))
	if summary == "" {
		return errors.New("the model returned no summary")
	}
	fresh := []Message{
		{Role: "user", Content: []Block{{Type: "text", Text: summaryHeader + "\n" + summary}}},
		{Role: "assistant", Content: []Block{{Type: "text", Text: "Understood. Continuing from this summary."}}},
	}
	s.mu.Lock()
	// only this goroutine mutates history while busy, so msgs[idx:] is still current
	s.msgs = append(fresh, s.msgs[idx:]...)
	s.mu.Unlock()
	s.emit(Event{Type: "compact", Text: summary})
	return nil
}

// CompactNow is the manual "Compact" button.
func (s *Session) CompactNow() error {
	ctx, ok := s.begin()
	if !ok {
		return errBusy
	}
	s.mu.Lock()
	profile, model := s.Profile, s.modelLocked()
	s.mu.Unlock()
	prov, err := s.srv.provider(profile)
	if err != nil {
		s.end()
		return err
	}
	s.emit(Event{Type: "status", Text: "busy"})
	prov, model = s.srv.compactWith(prov, model)
	go func() {
		defer s.end()
		s.prepare()
		if err := s.compact(ctx, prov, model, toolDefs(), true); err != nil {
			s.emit(Event{Type: "error", Text: "compaction failed: " + err.Error(), Err: true})
		}
	}()
	return nil
}

// ------------------------------------------------------------- persistence

func (s *Session) save() {
	s.mu.Lock()
	if len(s.msgs) == 0 {
		s.mu.Unlock()
		return
	}
	data, err := json.Marshal(stored{ID: s.ID, Title: s.title, Profile: s.Profile, Model: s.Model, Mode: s.Mode,
		Workspace: s.Workspace, Updated: time.Now().Unix(), Usage: s.usage, LastIn: s.lastIn, Started: s.started, Msgs: s.msgs})
	s.mu.Unlock()
	if err != nil {
		return
	}
	path := s.srv.sessionPath(s.ID)
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		return
	}
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, path)
	}
}

// eventsFromHistory rebuilds a timeline for a session loaded from disk.
func eventsFromHistory(msgs []Message) []Event {
	var evs []Event
	add := func(e Event) {
		e.N = len(evs) + 1
		evs = append(evs, e)
	}
	for _, m := range msgs {
		if m.Role == "user" && hasText(m) {
			texts, imgs := imageBlocks(m)
			t := strings.Join(texts, "\n")
			if strings.HasPrefix(t, summaryHeader) {
				add(Event{Type: "compact", Text: strings.TrimSpace(strings.TrimPrefix(t, summaryHeader))})
				continue
			}
			add(Event{Type: "user", Text: t, Imgs: len(imgs)})
		}
		for _, b := range m.Content {
			switch {
			case b.Type == "text" && m.Role == "assistant" && strings.TrimSpace(b.Text) != "":
				add(Event{Type: "assistant", Text: b.Text})
			case b.Type == "tool_use":
				add(Event{Type: "tool_call", ID: b.ID, Name: b.Name, Text: toolSummary(b.Name, b.Input)})
			case b.Type == "tool_result":
				add(Event{Type: "tool_result", ID: b.ToolUseID, Text: trunc(b.Content, 3000), Err: b.IsError})
			}
		}
	}
	return evs
}

// ------------------------------------------------------------------ utils

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return trunc(s, 200)
}
