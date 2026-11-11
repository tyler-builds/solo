// Package agenttest provides a fake solo-agent sidecar for tests. It speaks the real protocol with
// scripted behavior instead of running Claude.
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
// and starts it with the command from Command. The message text picks the behavior:
//
//	write <path> <content...>  writes the file under the session's cwd, then completes
//	tool <name> <json args>    calls tool.call; the turn's result is the tool's text
//	ask                        calls question.ask; the result is the first answer
//	cancel                     calls question.ask and cancels it with $/cancel
//	slow                       runs until interrupted (status "interrupted")
//	crash                      starts a turn and exits the process
//	fail                       ends the turn with status "failed"
//	lose                       fails as Claude does when a resumed conversation is missing
//	bg <ms> <description...>   starts a background command and completes; after ms the command
//	                           finishes and, as Claude does, a turn of its own ("wake") reads it.
//	                           session.stopTask stops it first, with no turn after.
//	kill                       stops every background command inside the turn, as Claude's TaskStop
//	slowclose <ms>             completes; closing the session later takes ms (and holds up every other
//	                           request meanwhile, as one busy sidecar would)
//	anything else              completes with result "echo: <text> resume=<bool>"
//
// It does not import package agent, so agent's own tests can use it.
package agenttest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const envVar = "SOLO_FAKE_SIDECAR"

// Main runs the fake sidecar if this process was started as one, and reports whether it did.
func Main() bool {
	if os.Getenv(envVar) != "1" {
		return false
	}
	run()
	os.Exit(0)
	return true
}

// Command returns the executable and environment that start the fake: the current test binary with the
// fake enabled.
func Command() (path string, env []string) {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return exe, []string{envVar + "=1"}
}

type fakeSession struct {
	cwd       string
	resumed   bool
	seq       int
	interrupt chan struct{}
	// steer reaches a "slow" turn waiting on it (unbuffered: a steer with no such turn is a plain send).
	steer chan steerMsg

	bgMu   sync.Mutex
	bg     []*fakeTask // running background commands, oldest first
	closed bool
	// closeDelay is how long session.close takes (slowclose).
	closeDelay time.Duration
}

type fakeTask struct {
	id, desc string
	stop     chan struct{}
}

// startBg adds a background command; tasks lists those still running, as a background event does.
func (s *fakeSession) startBg(id, desc string) *fakeTask {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	t := &fakeTask{id: id, desc: desc, stop: make(chan struct{})}
	s.bg = append(s.bg, t)
	return t
}

// endBg removes a background command, and reports whether it was still running and the session open.
func (s *fakeSession) endBg(id string) bool {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	for i, t := range s.bg {
		if t.id == id {
			s.bg = append(s.bg[:i], s.bg[i+1:]...)
			return !s.closed
		}
	}
	return false
}

func (s *fakeSession) tasks() []any {
	s.bgMu.Lock()
	defer s.bgMu.Unlock()
	out := []any{}
	for _, t := range s.bg {
		out = append(out, map[string]any{"taskId": t.id, "taskType": "local_bash", "description": t.desc})
	}
	return out
}

type steerMsg struct{ uuid, text string }

func run() {
	var wm sync.Mutex
	write := func(v any) {
		b, _ := json.Marshal(v)
		wm.Lock()
		os.Stdout.Write(append(b, '\n'))
		wm.Unlock()
	}
	var mu sync.Mutex
	sessions := map[string]*fakeSession{}
	pending := map[int64]chan json.RawMessage{}
	nextID := int64(1000)

	event := func(sid string, body map[string]any) {
		mu.Lock()
		s := sessions[sid]
		if s == nil {
			mu.Unlock()
			return
		}
		s.seq++
		body["sessionId"], body["seq"] = sid, s.seq
		mu.Unlock()
		write(map[string]any{"method": "event", "params": body})
	}
	request := func(method string, params any) (int64, chan json.RawMessage) {
		mu.Lock()
		nextID++
		id := nextID
		ch := make(chan json.RawMessage, 1)
		pending[id] = ch
		mu.Unlock()
		write(map[string]any{"id": id, "method": method, "params": params})
		return id, ch
	}

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		var m struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			continue
		}
		if m.Method == "" && m.ID != nil {
			mu.Lock()
			ch := pending[*m.ID]
			delete(pending, *m.ID)
			mu.Unlock()
			if ch != nil {
				ch <- m.Result
			}
			continue
		}
		if m.ID == nil {
			continue
		}
		id := *m.ID
		reply := func(v any) { write(map[string]any{"id": id, "result": v}) }
		var p struct {
			SessionID string `json:"sessionId"`
			Resume    bool   `json:"resume"`
			Cwd       string `json:"cwd"`
			UUID      string `json:"uuid"`
			Text      string `json:"text"`
			Model     string `json:"model"`
			Prompt    string `json:"prompt"`
			Steer     bool   `json:"steer"`
			Images    []struct {
				MediaType string `json:"mediaType"`
			} `json:"images"`
		}
		json.Unmarshal(m.Params, &p)
		switch m.Method {
		case "hello":
			reply(map[string]any{"protocol": 1, "runtime": "fake"})
		case "session.open":
			mu.Lock()
			sessions[p.SessionID] = &fakeSession{cwd: p.Cwd, resumed: p.Resume, interrupt: make(chan struct{}, 1), steer: make(chan steerMsg)}
			mu.Unlock()
			reply(struct{}{})
			event(p.SessionID, map[string]any{"kind": "init", "model": "fake-model", "cwd": p.Cwd, "claudeVersion": "0.0.0", "permissionMode": "fake"})
		case "session.send":
			mu.Lock()
			s := sessions[p.SessionID]
			mu.Unlock()
			if s == nil {
				write(map[string]any{"id": id, "error": map[string]any{"code": -32001, "message": "no session"}})
				continue
			}
			reply(struct{}{})
			text := p.Text
			for _, im := range p.Images { // the echo reports the images it got
				text += " +" + im.MediaType
			}
			if p.Steer {
				select {
				case s.steer <- steerMsg{p.UUID, text}:
					continue
				default:
				}
			}
			go turn(s, p.SessionID, p.UUID, text, event, request, write)
		case "session.interrupt":
			mu.Lock()
			s := sessions[p.SessionID]
			mu.Unlock()
			if s != nil {
				select {
				case s.interrupt <- struct{}{}:
				default:
				}
			}
			reply(map[string]any{"stillQueued": []string{}})
		case "session.stopTask":
			var q struct {
				TaskID string `json:"taskId"`
			}
			json.Unmarshal(m.Params, &q)
			mu.Lock()
			s := sessions[p.SessionID]
			mu.Unlock()
			if s != nil {
				s.bgMu.Lock()
				for _, t := range s.bg {
					if t.id == q.TaskID {
						select {
						case <-t.stop:
						default:
							close(t.stop)
						}
					}
				}
				s.bgMu.Unlock()
			}
			reply(struct{}{})
		case "session.close":
			mu.Lock()
			s := sessions[p.SessionID]
			mu.Unlock()
			if s != nil {
				s.bgMu.Lock()
				s.closed = true // its background commands die with it, silently
				delay := s.closeDelay
				s.bgMu.Unlock()
				time.Sleep(delay)
			}
			event(p.SessionID, map[string]any{"kind": "exited"})
			mu.Lock()
			delete(sessions, p.SessionID)
			mu.Unlock()
			reply(struct{}{})
		case "session.setModel":
			reply(struct{}{})
		case "complete":
			// Names a branch from the "Task title:" line, or answers badly when asked to.
			title := ""
			for _, l := range strings.Split(p.Prompt, "\n") {
				if v, ok := strings.CutPrefix(l, "Task title: "); ok {
					title = v
				}
			}
			if strings.HasPrefix(p.Prompt, "Write the pull request") {
				reply(map[string]any{"text": "TITLE: " + title + "\nDESCRIPTION:\nDid it."})
				continue
			}
			if strings.Contains(p.Prompt, "BADNAME") {
				reply(map[string]any{"text": "`Here you go: feat/Odd Name~^:`"})
				continue
			}
			reply(map[string]any{"text": "feat/" + strings.ReplaceAll(strings.ToLower(title), " ", "-")})
		case "probe":
			reply(map[string]any{"claudeVersion": "0.0.0", "models": []any{}, "account": map[string]any{}})
		case "shutdown":
			reply(struct{}{})
			return
		default:
			write(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "unknown method"}})
		}
	}
}

func turn(s *fakeSession, sid, uuid, text string, event func(string, map[string]any),
	request func(string, any) (int64, chan json.RawMessage), write func(any)) {
	uuids := []string{uuid}
	if uuid == "" {
		uuids = []string{} // a turn Claude starts itself
	}
	event(sid, map[string]any{"kind": "turn_start", "uuids": uuids})
	status, result, errText := "completed", "", ""
	cmd, rest, _ := strings.Cut(text, " ")
	switch cmd {
	case "crash":
		os.Exit(3)
	case "write":
		path, content, _ := strings.Cut(rest, " ")
		full := filepath.Join(s.cwd, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content+"\n"), 0o644); err != nil {
			status, errText = "failed", err.Error()
		}
		toolID := fmt.Sprintf("toolu_%d", time.Now().UnixNano())
		event(sid, map[string]any{"kind": "assistant", "messageId": "m-" + uuid, "blocks": []any{
			map[string]any{"type": "tool_use", "id": toolID, "name": "Write", "input": map[string]any{"file_path": full}},
		}})
		event(sid, map[string]any{"kind": "tool_result", "toolUseId": toolID, "text": "File created"})
		result = "wrote " + path
	case "bash":
		// Runs rest with sh in the session's folder, as Claude's Bash tool does.
		toolID := fmt.Sprintf("toolu_%d", time.Now().UnixNano())
		event(sid, map[string]any{"kind": "assistant", "messageId": "m-" + uuid, "blocks": []any{
			map[string]any{"type": "tool_use", "id": toolID, "name": "Bash", "input": map[string]any{"command": rest}},
		}})
		cmd := exec.Command("sh", "-c", rest)
		cmd.Dir = s.cwd
		out, err := cmd.CombinedOutput()
		event(sid, map[string]any{"kind": "tool_result", "toolUseId": toolID, "text": string(out), "isError": err != nil})
		result = "ran it"
	case "tool":
		name, args, _ := strings.Cut(rest, " ")
		if args == "" {
			args = "{}"
		}
		_, ch := request("tool.call", map[string]any{"sessionId": sid, "name": name, "args": json.RawMessage(args)})
		var r struct {
			Text string `json:"text"`
		}
		json.Unmarshal(<-ch, &r)
		result = r.Text
	case "ask":
		_, ch := request("question.ask", map[string]any{"sessionId": sid, "toolUseId": "toolu_ask", "questions": []any{
			map[string]any{"question": "Tabs or spaces?", "header": "Indent", "multiSelect": false,
				"options": []any{map[string]any{"label": "Tabs", "description": "t"}, map[string]any{"label": "Spaces", "description": "s"}}},
		}})
		var r struct {
			Answers   map[string]string `json:"answers"`
			Cancelled bool              `json:"cancelled"`
		}
		json.Unmarshal(<-ch, &r)
		result = r.Answers["Tabs or spaces?"]
		if r.Cancelled {
			result = "cancelled"
		}
	case "cancel":
		id, _ := request("question.ask", map[string]any{"sessionId": sid, "toolUseId": "toolu_c", "questions": []any{
			map[string]any{"question": "Q?", "header": "Q", "multiSelect": false, "options": []any{}},
		}})
		time.Sleep(50 * time.Millisecond)
		write(map[string]any{"method": "$/cancel", "params": map[string]any{"id": id}})
		result = "cancelled"
	case "slow":
		select {
		case <-s.interrupt:
			status = "interrupted"
		case m := <-s.steer:
			// As Claude does: the turn ends aborted, marked steered, and carries on with the steer.
			event(sid, map[string]any{"kind": "turn_end", "uuids": []string{uuid}, "status": "interrupted",
				"reason": "aborted_streaming", "steered": true, "usage": map[string]any{}})
			turn(s, sid, m.uuid, m.text, event, request, write)
			return
		case <-time.After(30 * time.Second):
		}
	case "bg":
		msText, desc, _ := strings.Cut(rest, " ")
		ms, _ := strconv.Atoi(msText)
		id := fmt.Sprintf("b%d", time.Now().UnixNano())
		toolID := "toolu_" + id
		event(sid, map[string]any{"kind": "assistant", "messageId": "m-" + uuid, "blocks": []any{
			map[string]any{"type": "tool_use", "id": toolID, "name": "Bash",
				"input": map[string]any{"command": "sleep", "description": desc, "run_in_background": true}},
		}})
		t := s.startBg(id, desc)
		event(sid, map[string]any{"kind": "background", "tasks": s.tasks()})
		event(sid, map[string]any{"kind": "task", "phase": "started", "taskId": id, "toolUseId": toolID,
			"taskType": "local_bash", "description": desc, "background": true})
		event(sid, map[string]any{"kind": "tool_result", "toolUseId": toolID, "text": "Command running in background with ID: " + id})
		go func() {
			ended := "completed"
			select {
			case <-t.stop:
				ended = "stopped"
			case <-time.After(time.Duration(ms) * time.Millisecond):
			}
			if !s.endBg(id) {
				return
			}
			event(sid, map[string]any{"kind": "task", "phase": "ended", "taskId": id, "toolUseId": toolID, "status": ended})
			event(sid, map[string]any{"kind": "background", "tasks": s.tasks()})
			if ended == "completed" {
				turn(s, sid, "", "wake "+desc, event, request, write)
			}
		}()
		result = "started " + desc
	case "kill":
		s.bgMu.Lock()
		running := append([]*fakeTask(nil), s.bg...)
		s.bgMu.Unlock()
		for _, t := range running {
			if s.endBg(t.id) {
				event(sid, map[string]any{"kind": "task", "phase": "ended", "taskId": t.id, "status": "stopped"})
				event(sid, map[string]any{"kind": "background", "tasks": s.tasks()})
			}
		}
		result = fmt.Sprintf("stopped %d", len(running))
	case "slowclose":
		ms, _ := strconv.Atoi(rest)
		s.bgMu.Lock()
		s.closeDelay = time.Duration(ms) * time.Millisecond
		s.bgMu.Unlock()
		result = "slow to close"
	case "fail":
		status, errText = "failed", "scripted failure"
	case "lose":
		status, errText = "failed", "No conversation found with session ID: "+sid
	default:
		result = fmt.Sprintf("echo: %s resume=%v", text, s.resumed)
	}
	if status == "completed" {
		event(sid, map[string]any{"kind": "text_delta", "messageId": "m2-" + uuid, "index": 0, "text": result})
		event(sid, map[string]any{"kind": "assistant", "messageId": "m2-" + uuid, "blocks": []any{map[string]any{"type": "text", "text": result}}})
	}
	end := map[string]any{"kind": "turn_end", "uuids": []string{uuid}, "status": status, "totalCostUsd": 0.01,
		"usage": map[string]any{}, "contextTokens": 1000}
	if status == "completed" {
		end["result"] = result
	}
	if errText != "" {
		end["error"] = errText
	}
	event(sid, end)
}
