// Package agenttest provides a fake claude CLI for tests: the test binary, started as claude (with its
// stream-json flags), speaks Claude Code's control protocol on stdin and stdout with scripted behavior
// instead of running Claude. One process is one session, as with the real CLI.
//
// A test package enables it from TestMain:
//
//	func TestMain(m *testing.M) {
//		if agenttest.Main() {
//			return
//		}
//		os.Exit(m.Run())
//	}
//
// and runs it as Solo's claude (core.Config.ClaudePath = ClaudePath()). The message text picks the
// behavior:
//
//	write <path> <content...>  writes the file under the session's cwd, then completes
//	bash <command...>          runs the command with sh in the session's cwd, then completes
//	tool <name> <json args>    calls the app's tool (mcp__solo__<name>); the turn's result is its text
//	mcp <name> <json args>     the same, shown in the transcript as Claude shows an MCP tool call
//	                           (its tool_use and its tool_result)
//	ask                        asks AskUserQuestion; the result is the first answer
//	cancel                     asks AskUserQuestion and withdraws it (control_cancel_request)
//	slow                       runs until interrupted (status "interrupted"), or steered: it ends
//	                           aborted and carries on with the steer, as Claude does
//	crash                      starts a turn and exits the process
//	fail                       ends the turn failed
//	lose                       fails as Claude does when a resumed conversation is missing
//	bg <ms> <description...>   starts a background command and completes; after ms the command
//	                           finishes and, as Claude does, a turn of its own ("wake") reads it.
//	                           stop_task stops it first, with no turn after. A description that starts
//	                           with "quiet" wakes to a turn with nothing new to say: "(noted)".
//	kill                       stops every background command inside the turn, as Claude's TaskStop
//	slowclose <ms>             completes; closing the session later takes ms
//	Write the pull request …   in a session started with skills: writes pull request text as Claude
//	                           would with them (see skillTurn)
//	a board card lead's message (core/card.go): calls its card tool, as a lead would (leadAction)
//	anything else              completes with result "echo: <text> resume=<bool>"
//
// Started for a one-shot call (claudesdk.Complete: --max-turns), it names a branch from the prompt's
// "Task title:" line, or writes pull request text. With --version, it prints a version.
//
// It does not import package agent, so agent's own tests can use it.
package agenttest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/proc"
)

// Main runs the fake claude if this process was started as one, and reports whether it did.
func Main() bool {
	if !isClaude() {
		return false
	}
	run()
	os.Exit(0)
	return true
}

// ClaudePath is the fake claude: the current test binary.
func ClaudePath() string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return exe
}

func isClaude() bool {
	return slices.Contains(os.Args[1:], "--input-format") || (len(os.Args) == 2 && os.Args[1] == "--version")
}

type fake struct {
	cwd     string
	resumed bool
	oneShot bool     // started by claudesdk.Complete
	mcpName string   // the app's in-process MCP server
	skills  []string // from initialize

	wmu sync.Mutex

	mu         sync.Mutex
	pending    map[string]chan json.RawMessage
	nextID     int
	stopped    map[string]chan struct{} // background commands, by task id
	bg         []bgTask                 // running background commands, oldest first
	closeDelay time.Duration

	interrupt chan struct{}
	steer     chan steerMsg // reaches a "slow" turn waiting on it
	turns     chan turnMsg  // turns run one at a time, as Claude runs them
}

type bgTask struct{ id, desc string }

type steerMsg struct{ uuid, text string }

type turnMsg struct{ uuid, text string }

func run() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("0.0.0 (Claude Code)")
		return
	}
	f := &fake{pending: map[string]chan json.RawMessage{}, stopped: map[string]chan struct{}{},
		interrupt: make(chan struct{}, 1), steer: make(chan steerMsg), turns: make(chan turnMsg, 64), mcpName: "solo"}
	f.cwd, _ = os.Getwd()
	for i, a := range os.Args {
		switch {
		case strings.HasPrefix(a, "--resume="):
			f.resumed = true
		case a == "--max-turns":
			f.oneShot = true
		case a == "--mcp-config" && i+1 < len(os.Args):
			var cfg struct {
				Servers map[string]struct {
					Type string `json:"type"`
				} `json:"mcpServers"`
			}
			if json.Unmarshal([]byte(os.Args[i+1]), &cfg) == nil {
				for name, s := range cfg.Servers {
					if s.Type == "sdk" {
						f.mcpName = name
					}
				}
			}
		}
	}
	go f.runTurns()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		var m map[string]any
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		switch m["type"] {
		case "control_request":
			f.control(m)
		case "control_response":
			r, _ := m["response"].(map[string]any)
			id, _ := r["request_id"].(string)
			b, _ := json.Marshal(r["response"])
			f.mu.Lock()
			ch := f.pending[id]
			delete(f.pending, id)
			f.mu.Unlock()
			if ch != nil {
				ch <- b
			}
		case "user":
			uuid, _ := m["uuid"].(string)
			text := userText(m)
			if m["priority"] == "now" {
				select {
				case f.steer <- steerMsg{uuid, text}:
					continue
				default:
				}
			}
			f.turns <- turnMsg{uuid, text}
		}
	}
	// stdin closed: the session is closing; its background commands die with it, silently.
	f.mu.Lock()
	delay := f.closeDelay
	f.mu.Unlock()
	time.Sleep(delay)
}

// userText is a user message's text; the echo reports the images it got.
func userText(m map[string]any) string {
	msg, _ := m["message"].(map[string]any)
	if s, ok := msg["content"].(string); ok {
		return s
	}
	text, imgs := "", ""
	blocks, _ := msg["content"].([]any)
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		switch bm["type"] {
		case "text":
			text, _ = bm["text"].(string)
		case "image":
			src, _ := bm["source"].(map[string]any)
			mt, _ := src["media_type"].(string)
			imgs += " +" + mt
		}
	}
	return text + imgs
}

func (f *fake) write(v any) {
	b, _ := json.Marshal(v)
	f.wmu.Lock()
	os.Stdout.Write(append(b, '\n'))
	f.wmu.Unlock()
}

func (f *fake) control(m map[string]any) {
	req, _ := m["request"].(map[string]any)
	resp := map[string]any{}
	initialized := false
	switch req["subtype"] {
	case "initialize":
		if list, ok := req["skills"].([]any); ok {
			for _, s := range list {
				if name, ok := s.(string); ok {
					f.skills = append(f.skills, name)
				}
			}
		}
		resp = map[string]any{"models": []any{}, "account": map[string]any{}}
		initialized = true
	case "interrupt":
		select {
		case f.interrupt <- struct{}{}:
		default:
		}
		resp = map[string]any{"still_queued": []string{}}
	case "stop_task":
		id, _ := req["task_id"].(string)
		f.mu.Lock()
		if ch := f.stopped[id]; ch != nil {
			select {
			case <-ch:
			default:
				close(ch)
			}
		}
		f.mu.Unlock()
	case "background_tasks":
		resp = map[string]any{"backgrounded": true}
	}
	f.write(map[string]any{"type": "control_response", "response": map[string]any{
		"subtype": "success", "request_id": m["request_id"], "response": resp}})
	if initialized && !f.oneShot {
		f.system("init", map[string]any{"model": "fake-model", "cwd": f.cwd, "claude_code_version": "0.0.0", "permissionMode": "fake"})
	}
}

// request sends the app a control request and returns its id and a channel for the answer.
func (f *fake) request(req map[string]any) (string, chan json.RawMessage) {
	f.mu.Lock()
	f.nextID++
	id := fmt.Sprintf("cli_%d", f.nextID)
	ch := make(chan json.RawMessage, 1)
	f.pending[id] = ch
	f.mu.Unlock()
	f.write(map[string]any{"type": "control_request", "request_id": id, "request": req})
	return id, ch
}

// callTool calls one of the app's in-process MCP tools.
func (f *fake) callTool(server, name string, args json.RawMessage) (string, bool) {
	f.mu.Lock()
	f.nextID++
	n := f.nextID
	f.mu.Unlock()
	_, ch := f.request(map[string]any{"subtype": "mcp_message", "server_name": server,
		"message": map[string]any{"jsonrpc": "2.0", "id": n, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}}})
	var r struct {
		MCP struct {
			Result struct {
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
				IsError bool `json:"isError"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		} `json:"mcp_response"`
	}
	_ = json.Unmarshal(<-ch, &r)
	if r.MCP.Error != nil {
		return r.MCP.Error.Message, true
	}
	text := ""
	if len(r.MCP.Result.Content) > 0 {
		text = r.MCP.Result.Content[0].Text
	}
	return text, r.MCP.Result.IsError
}

// ask asks AskUserQuestion, as the CLI does: a permission request. It returns the request's id and a
// channel for the answer.
func (f *fake) ask(toolUseID string, questions []any) (string, chan json.RawMessage) {
	return f.request(map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "tool_use_id": toolUseID,
		"input": map[string]any{"questions": questions}})
}

func toolID() string { return fmt.Sprintf("toolu_%d", time.Now().UnixNano()) }

func (f *fake) toolUse(id, name string, input any) {
	f.write(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": "m-" + id,
		"content": []any{map[string]any{"type": "tool_use", "id": id, "name": name, "input": input}}}})
}

func (f *fake) toolResult(id, text string, isErr bool) {
	f.write(map[string]any{"type": "user", "parent_tool_use_id": nil, "message": map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "tool_result", "tool_use_id": id, "content": text, "is_error": isErr}}}})
}

func (f *fake) system(subtype string, fields map[string]any) {
	fields["type"], fields["subtype"] = "system", subtype
	f.write(fields)
}

func (f *fake) tasksChanged() {
	f.mu.Lock()
	tasks := []any{}
	for _, t := range f.bg {
		tasks = append(tasks, map[string]any{"task_id": t.id, "task_type": "local_bash", "description": t.desc})
	}
	f.mu.Unlock()
	f.system("background_tasks_changed", map[string]any{"tasks": tasks})
}

// endBg removes a background command, reporting whether it was still running.
func (f *fake) endBg(id string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, t := range f.bg {
		if t.id == id {
			f.bg = append(f.bg[:i], f.bg[i+1:]...)
			delete(f.stopped, id)
			return true
		}
	}
	return false
}

func (f *fake) runTurns() {
	for t := range f.turns {
		f.turn(t.uuid, t.text)
	}
}

// end finishes a turn: its text (a message as Claude streams it), then its result.
func (f *fake) end(uuid, status, result, errText string) {
	uuids := []string{}
	if uuid != "" {
		uuids = []string{uuid}
	}
	if status == "completed" {
		id := "m2-" + toolID()
		f.write(map[string]any{"type": "stream_event", "parent_tool_use_id": nil,
			"event": map[string]any{"type": "message_start", "message": map[string]any{"id": id, "usage": map[string]any{"input_tokens": 1000}}}})
		f.write(map[string]any{"type": "stream_event", "parent_tool_use_id": nil,
			"event": map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": result}}})
		f.write(map[string]any{"type": "assistant", "parent_tool_use_id": nil, "message": map[string]any{"id": id,
			"content": []any{map[string]any{"type": "text", "text": result}}}})
	}
	r := map[string]any{"type": "result", "subtype": "success", "result": result, "user_message_uuids": uuids,
		"total_cost_usd": 0.01, "num_turns": 1, "usage": map[string]any{}}
	switch status {
	case "failed":
		r["subtype"], r["errors"], r["result"] = "error_during_execution", []string{errText}, ""
	case "interrupted":
		r["terminal_reason"], r["result"] = "aborted_streaming", ""
	}
	f.write(r)
}

func (f *fake) turn(uuid, text string) {
	if uuid != "" {
		// The CLI echoes the message it takes; its uuid opens the turn.
		f.write(map[string]any{"type": "user", "uuid": uuid, "parent_tool_use_id": nil, "message": map[string]any{"role": "user", "content": text}})
	}
	if f.oneShot {
		f.end(uuid, "completed", completeAnswer(text), "")
		return
	}
	status, result, errText := "completed", "", ""
	cmd, rest, _ := strings.Cut(text, " ")
	switch cmd {
	case "crash":
		os.Exit(3)
	case "write":
		path, content, _ := strings.Cut(rest, " ")
		full := filepath.Join(f.cwd, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content+"\n"), 0o644); err != nil {
			status, errText = "failed", err.Error()
		}
		id := toolID()
		f.toolUse(id, "Write", map[string]any{"file_path": full})
		f.toolResult(id, "File created", false)
		result = "wrote " + path
	case "bash":
		runCommand(f, f.cwd, rest, nil)
		result = "ran it"
	case "tool", "mcp":
		name, args, _ := strings.Cut(rest, " ")
		if args == "" {
			args = "{}"
		}
		id := toolID()
		if cmd == "mcp" {
			f.toolUse(id, "mcp__"+f.mcpName+"__"+name, json.RawMessage(args))
		}
		text, isErr := f.callTool(f.mcpName, name, json.RawMessage(args))
		if cmd == "mcp" {
			f.toolResult(id, text, isErr)
		}
		result = text
	case "ask":
		_, ch := f.ask("toolu_ask", []any{map[string]any{"question": "Tabs or spaces?", "header": "Indent", "multiSelect": false,
			"options": []any{map[string]any{"label": "Tabs", "description": "t"}, map[string]any{"label": "Spaces", "description": "s"}}}})
		var r struct {
			Behavior     string `json:"behavior"`
			UpdatedInput struct {
				Answers map[string]string `json:"answers"`
			} `json:"updatedInput"`
		}
		_ = json.Unmarshal(<-ch, &r)
		result = r.UpdatedInput.Answers["Tabs or spaces?"]
		if r.Behavior != "allow" {
			result = "cancelled"
		}
	case "cancel":
		id, _ := f.ask("toolu_c", []any{map[string]any{"question": "Q?", "header": "Q", "multiSelect": false, "options": []any{}}})
		time.Sleep(50 * time.Millisecond)
		f.write(map[string]any{"type": "control_cancel_request", "request_id": id})
		result = "cancelled"
	case "slow":
		select {
		case <-f.interrupt:
			status = "interrupted"
		case m := <-f.steer:
			// As Claude does: the turn ends aborted, and carries on with the steer.
			f.end(uuid, "interrupted", "", "")
			f.turn(m.uuid, m.text)
			return
		case <-time.After(30 * time.Second):
		}
	case "bg":
		msText, desc, _ := strings.Cut(rest, " ")
		ms, _ := strconv.Atoi(msText)
		id := fmt.Sprintf("b%d", time.Now().UnixNano())
		tu := "toolu_" + id
		f.toolUse(tu, "Bash", map[string]any{"command": "sleep", "description": desc, "run_in_background": true})
		stop := make(chan struct{})
		f.mu.Lock()
		f.bg = append(f.bg, bgTask{id, desc})
		f.stopped[id] = stop
		f.mu.Unlock()
		f.tasksChanged()
		f.system("task_started", map[string]any{"task_id": id, "tool_use_id": tu, "task_type": "local_bash", "description": desc, "is_backgrounded": true})
		f.toolResult(tu, "Command running in background with ID: "+id, false)
		go func() {
			ended := "completed"
			select {
			case <-stop:
				ended = "stopped"
			case <-time.After(time.Duration(ms) * time.Millisecond):
			}
			if !f.endBg(id) {
				return
			}
			f.system("task_notification", map[string]any{"task_id": id, "tool_use_id": tu, "status": ended})
			f.tasksChanged()
			if ended == "completed" {
				f.turns <- turnMsg{"", "wake " + desc} // as Claude does: a turn of its own reads the result
			}
		}()
		result = "started " + desc
	case "kill":
		f.mu.Lock()
		running := append([]bgTask(nil), f.bg...)
		f.mu.Unlock()
		for _, t := range running {
			if f.endBg(t.id) {
				f.system("task_notification", map[string]any{"task_id": t.id, "status": "stopped"})
				f.tasksChanged()
			}
		}
		result = fmt.Sprintf("stopped %d", len(running))
	case "slowclose":
		ms, _ := strconv.Atoi(rest)
		f.mu.Lock()
		f.closeDelay = time.Duration(ms) * time.Millisecond
		f.mu.Unlock()
		result = "slow to close"
	case "Write":
		if len(f.skills) > 0 && strings.HasPrefix(text, "Write the pull request") {
			result = skillTurn(f, text)
		} else {
			result = fmt.Sprintf("echo: %s resume=%v", text, f.resumed)
		}
	case "wake":
		result = fmt.Sprintf("echo: %s resume=%v", text, f.resumed)
		if strings.HasPrefix(rest, "quiet") {
			result = "(noted)"
		}
	case "fail":
		status, errText = "failed", "scripted failure"
	case "lose":
		status, errText = "failed", "No conversation found with session ID: fake"
	default:
		result = fmt.Sprintf("echo: %s resume=%v", text, f.resumed)
		if tool, args := leadAction(text); tool != "" {
			id := toolID()
			f.toolUse(id, "mcp__card__"+tool, args)
			b, _ := json.Marshal(args)
			res, isErr := f.callTool("card", tool, b)
			f.toolResult(id, res, isErr)
			result = "Done."
		}
	}
	f.end(uuid, status, result, errText)
}

// completeAnswer answers a one-shot call: pull request text, or a branch named from the "Task title:"
// line (badly, when asked to).
func completeAnswer(prompt string) string {
	title := ""
	for _, l := range strings.Split(prompt, "\n") {
		if v, ok := strings.CutPrefix(l, "Task title: "); ok {
			title = v
		}
	}
	switch {
	case strings.HasPrefix(prompt, "Write the pull request"):
		return "TITLE: " + title + "\nDESCRIPTION:\nDid it."
	case strings.Contains(prompt, "BADNAME"):
		return "`Here you go: feat/Odd Name~^:`"
	}
	return "feat/" + strings.ReplaceAll(strings.ToLower(title), " ", "-")
}

// leadAction is the card tool a board card's lead calls for a message from Solo, or "" for a message
// that isn't to a lead: it briefs the builder with the user's words, answers its questions with
// "Tabs", and reviews its work by quoting its report.
func leadAction(text string) (string, map[string]any) {
	para := func(prefix string) string {
		rest := strings.TrimLeft(strings.TrimPrefix(text, prefix), "\n")
		if i := strings.Index(rest, "\n\n"); i >= 0 {
			rest = rest[:i]
		}
		return strings.TrimSpace(rest)
	}
	switch {
	case strings.HasPrefix(text, "New card from the user:"):
		ask := para("New card from the user:")
		if strings.HasPrefix(ask, "unclear") {
			return "report", map[string]any{"message": "Needs you: which page?", "need": "input"}
		}
		return "brief_builder", map[string]any{"instructions": ask}
	case strings.HasPrefix(text, "The user replied:"):
		return "brief_builder", map[string]any{"instructions": para("The user replied:")}
	case strings.HasPrefix(text, "The builder asks:"):
		return "answer_builder", map[string]any{"answer": "Tabs"}
	}
	for _, marker := range []string{"The builder finished. Its report:", "The builder failed. Its report:"} {
		if i := strings.Index(text, marker); i >= 0 {
			text = text[i:]
			return "report", map[string]any{"message": "✓ Reviewed: " + para(marker), "need": "review"}
		}
	}
	return "", nil
}

// runCommand runs a command with sh in dir, as Claude's Bash tool does, with its tool use and result,
// and returns its output.
func runCommand(f *fake, dir, command string, env []string) string {
	id := toolID()
	f.toolUse(id, "Bash", map[string]any{"command": command})
	cmd := proc.Command("sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	f.toolResult(id, string(out), err != nil)
	return string(out)
}

// runLine finds the commands a SKILL.md gives as "Run `command`".
var runLine = regexp.MustCompile("Run `([^`]+)`")

// skillTurn writes pull request text as Claude would with the session's skills: it loads each from the
// user's skills folder ($CLAUDE_CONFIG_DIR or ~/.claude) or the project's (.claude/skills under the
// session's folder), runs the commands its SKILL.md gives as "Run `command`" in the session's folder
// with CLAUDE_SKILL_DIR set to the skill's folder, and answers, after a line about what it did, with
// the prompt's task title and the commands' output as the description.
func skillTurn(f *fake, text string) string {
	title := ""
	for _, l := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(l, "Task title: "); ok {
			title = v
		}
	}
	claude := os.Getenv("CLAUDE_CONFIG_DIR")
	if claude == "" {
		home, _ := os.UserHomeDir()
		claude = filepath.Join(home, ".claude")
	}
	var outs []string
	for _, name := range f.skills {
		for _, dir := range []string{filepath.Join(claude, "skills", name), filepath.Join(f.cwd, ".claude", "skills", name)} {
			body, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
			if err != nil {
				continue
			}
			id := toolID()
			f.toolUse(id, "Skill", map[string]any{"skill": name})
			f.toolResult(id, "Launching skill: "+name, false)
			for _, m := range runLine.FindAllStringSubmatch(string(body), -1) {
				outs = append(outs, strings.TrimSpace(runCommand(f, f.cwd, m[1], []string{"CLAUDE_SKILL_DIR=" + dir})))
			}
			break
		}
	}
	return "I used the skills.\n\nTITLE: " + title + "\nDESCRIPTION:\n" + strings.Join(outs, "\n")
}
