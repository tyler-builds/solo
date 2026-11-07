// Command solo-harness runs one Claude session from the terminal through the same agent layer the app
// uses: the sidecar, the session manager, its events, questions and tools. It is for testing that layer
// without the UI.
//
//	go run ./cmd/solo-harness -cwd ~/code/some-repo -model sonnet "fix the failing test"
//
// Type a line to send it. /interrupt stops the running turn, /quit exits. When Claude asks a question,
// answer with the option's number or with text.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/tyler-builds/solo/internal/agent"
)

func main() {
	cwd, _ := os.Getwd()
	dir := flag.String("cwd", cwd, "working directory for the session")
	model := flag.String("model", "", "model alias or ID (default: Claude's default)")
	readonly := flag.Bool("readonly", false, "read-only session (orchestrator policy) instead of bypassPermissions")
	resume := flag.String("resume", "", "resume this session ID")
	verbose := flag.Bool("v", false, "log the sidecar's stderr")
	probe := flag.Bool("probe", false, "print the models and account, then exit")
	flag.Parse()

	level := slog.LevelWarn
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	cmd, err := agent.FindCommand()
	if err != nil {
		fatal(err)
	}
	lines := make(chan string)
	go func() {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	turnDone := make(chan agent.Event, 1)
	questions := make(chan struct{}, 1)
	var answerCh = make(chan string)

	m := agent.NewManager(cmd, agent.Callbacks{
		OnEvent: func(e agent.Event) { printEvent(e, turnDone) },
		OnToolCall: func(ctx context.Context, p agent.ToolCallParams) (agent.ToolCallResult, error) {
			return agent.ToolCallResult{Text: "solo-harness has no orchestrator tools."}, nil
		},
		OnAsk: func(ctx context.Context, p agent.AskParams) (agent.AskResult, error) {
			answers := map[string]string{}
			for _, q := range p.Questions {
				fmt.Printf("\n\033[1;33m? %s\033[0m\n", q.Question)
				for i, o := range q.Options {
					fmt.Printf("  %d. %s — %s\n", i+1, o.Label, o.Description)
				}
				fmt.Print("answer> ")
				questions <- struct{}{}
				select {
				case a := <-answerCh:
					if n, err := strconv.Atoi(strings.TrimSpace(a)); err == nil && n >= 1 && n <= len(q.Options) {
						a = q.Options[n-1].Label
					}
					answers[q.Question] = a
				case <-ctx.Done():
					return agent.AskResult{Cancelled: true}, nil
				}
			}
			return agent.AskResult{Answers: answers}, nil
		},
	}, log)
	defer m.Shutdown()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	if *probe {
		res, err := m.Probe(ctx, agent.FindClaude(""))
		if err != nil {
			fatal(err)
		}
		b, _ := json.MarshalIndent(res, "", "  ")
		fmt.Println(string(b))
		return
	}

	p := agent.OpenParams{
		SessionID:      agent.NewUUID(),
		Cwd:            *dir,
		Model:          *model,
		PermissionMode: agent.PermissionBypass,
		ToolPolicy:     agent.ToolPolicyAllow,
		ClaudePath:     agent.FindClaude(""),
	}
	if *resume != "" {
		p.SessionID, p.Resume = *resume, true
	}
	if *readonly {
		p.PermissionMode, p.ToolPolicy = agent.PermissionDefault, agent.ToolPolicyReadOnly
		p.BashAllow = []string{`git (status|log|diff|show|branch)( .*)?`, `ls( .*)?`}
	}
	if err := m.Open(ctx, p); err != nil {
		fatal(err)
	}
	fmt.Printf("\033[2msession %s in %s\033[0m\n", p.SessionID, p.Cwd)

	busy := false
	send := func(text string) {
		if _, err := m.Send(ctx, p.SessionID, text, false); err != nil {
			fmt.Fprintln(os.Stderr, "send:", err)
			return
		}
		busy = true
	}
	if flag.NArg() > 0 {
		send(strings.Join(flag.Args(), " "))
	}
	asking := false
	for {
		if !busy && !asking {
			fmt.Print("\n> ")
		}
		select {
		case <-ctx.Done():
			if busy {
				_ = m.Interrupt(context.Background(), p.SessionID)
			}
			return
		case <-questions:
			asking = true
		case <-turnDone:
			busy = false
		case line, ok := <-lines:
			if !ok {
				return
			}
			if asking {
				asking = false
				answerCh <- line
				continue
			}
			switch strings.TrimSpace(line) {
			case "":
			case "/quit":
				return
			case "/interrupt":
				if err := m.Interrupt(ctx, p.SessionID); err != nil {
					fmt.Fprintln(os.Stderr, "interrupt:", err)
				}
			default:
				send(line)
			}
		}
	}
}

func printEvent(e agent.Event, turnDone chan<- agent.Event) {
	if !e.IsRoot() {
		return
	}
	switch e.Kind {
	case agent.EventInit:
		fmt.Printf("\033[2m[%s · claude %s · %s]\033[0m\n", e.Model, e.ClaudeVersion, e.PermissionMode)
	case agent.EventTextDelta:
		fmt.Print(e.Text)
	case agent.EventThinkingDelta:
		fmt.Print("\033[2m" + e.Text + "\033[0m")
	case agent.EventAssistant:
		for _, b := range e.Blocks {
			if b.Type == "tool_use" {
				fmt.Printf("\n\033[36m⏺ %s\033[0m \033[2m%s\033[0m\n", b.Name, clip(string(b.Input), 160))
			}
		}
		if e.Error != "" {
			fmt.Printf("\n\033[31m%s\033[0m\n", e.Error)
		}
	case agent.EventToolResult:
		mark := "\033[2m  ⎿ "
		if e.IsError {
			mark = "\033[31m  ⎿ "
		}
		fmt.Println(mark + clip(strings.ReplaceAll(e.Text, "\n", " "), 160) + "\033[0m")
	case agent.EventTask:
		if !e.Ambient && (e.Phase == "started" || e.Phase == "ended") {
			fmt.Printf("\033[35m  ◆ task %s %s %s\033[0m\n", e.Phase, e.Description, e.Status)
		}
	case agent.EventCompact:
		fmt.Printf("\033[2m[compacted %d → %d tokens]\033[0m\n", e.PreTokens, e.PostTokens)
	case agent.EventAPIRetry:
		fmt.Printf("\033[33m[retry %d/%d in %dms: %s]\033[0m\n", e.Attempt, e.MaxRetries, e.DelayMs, e.Error)
	case agent.EventTurnEnd:
		color := "32"
		if e.Status != agent.TurnCompleted {
			color = "31"
		}
		fmt.Printf("\n\033[%sm[%s", color, e.Status)
		if e.Reason != "" {
			fmt.Printf(" · %s", e.Reason)
		}
		fmt.Printf(" · %.1fs · $%.4f total · ctx %dk]\033[0m\n", float64(e.DurationMs)/1000, e.TotalCostUSD, e.ContextTokens/1000)
		if e.Error != "" {
			fmt.Printf("\033[31m%s\033[0m\n", e.Error)
		}
		select {
		case turnDone <- e:
		default:
		}
	case agent.EventExited:
		if e.Error != "" {
			fmt.Printf("\033[31m[claude exited: %s]\033[0m\n", e.Error)
		}
	}
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "solo-harness:", err)
	os.Exit(1)
}
