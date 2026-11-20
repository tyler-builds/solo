//go:build !js

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/tyler-builds/solo/internal/proc"
)

// Sidecar is one running solo-agent process.
type Sidecar struct {
	cmd  *exec.Cmd
	conn *conn
	in   io.WriteCloser
	exit chan struct{}
	err  error // set before exit closes
}

// Callbacks receive what the sidecar sends. OnEvent runs on the read goroutine, in order, and must not
// block for long; OnToolCall and OnAsk run on their own goroutines and may block.
type Callbacks struct {
	OnEvent    func(Event)
	OnToolCall func(ctx context.Context, p ToolCallParams) (ToolCallResult, error)
	OnAsk      func(ctx context.Context, p AskParams) (AskResult, error)
	// OnGuard checks a tool call an agent is about to make (the sidecar's PreToolUse hook).
	OnGuard func(ctx context.Context, p GuardParams) (GuardResult, error)
}

// Command is how to start the sidecar.
type Command struct {
	Path string
	Args []string
	Env  []string // added to the inherited environment
}

// StartSidecar starts the sidecar and checks its protocol version.
func StartSidecar(ctx context.Context, c Command, cb Callbacks, log *slog.Logger) (*Sidecar, error) {
	cmd := proc.Command(c.Path, c.Args...)
	cmd.Env = append(os.Environ(), c.Env...)
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start solo-agent: %w", err)
	}

	s := &Sidecar{cmd: cmd, in: in, exit: make(chan struct{})}
	s.conn = newConn(in)
	s.conn.onNotify("event", func(raw json.RawMessage) {
		var e Event
		if err := json.Unmarshal(raw, &e); err != nil {
			log.Warn("agent: bad event", "err", err)
			return
		}
		if cb.OnEvent != nil {
			cb.OnEvent(e)
		}
	})
	s.conn.handle("tool.call", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p ToolCallParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if cb.OnToolCall == nil {
			return ToolCallResult{Text: "This tool is not available.", IsError: true}, nil
		}
		return cb.OnToolCall(ctx, p)
	})
	s.conn.handle("tool.guard", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p GuardParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if cb.OnGuard == nil {
			return GuardResult{}, nil
		}
		return cb.OnGuard(ctx, p)
	})
	s.conn.handle("question.ask", func(ctx context.Context, raw json.RawMessage) (any, error) {
		var p AskParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if cb.OnAsk == nil {
			return AskResult{Cancelled: true}, nil
		}
		return cb.OnAsk(ctx, p)
	})

	var stderrTail tail
	go func() {
		sc := bufio.NewScanner(errPipe)
		sc.Buffer(make([]byte, 0, 4096), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			stderrTail.add(line)
			if strings.HasPrefix(line, "[solo-agent] warning: ") {
				log.Warn("solo-agent", "stderr", line)
			} else {
				log.Debug("solo-agent", "stderr", line)
			}
		}
	}()
	go s.conn.serve(out)
	go func() {
		werr := cmd.Wait()
		<-s.conn.done
		if werr != nil {
			s.err = fmt.Errorf("solo-agent exited: %w\n%s", werr, stderrTail.String())
		} else {
			s.err = errors.New("solo-agent exited")
		}
		close(s.exit)
	}()

	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var hello struct {
		Protocol int    `json:"protocol"`
		Runtime  string `json:"runtime"`
	}
	if err := s.conn.call(hctx, "hello", struct{}{}, &hello); err != nil {
		s.Kill()
		return nil, fmt.Errorf("solo-agent did not answer: %w\n%s", err, stderrTail.String())
	}
	if hello.Protocol != ProtocolVersion {
		s.Kill()
		return nil, fmt.Errorf("solo-agent speaks protocol %d, want %d", hello.Protocol, ProtocolVersion)
	}
	log.Info("agent: sidecar ready", "runtime", hello.Runtime, "pid", cmd.Process.Pid)
	return s, nil
}

// Exited is closed when the process has exited.
func (s *Sidecar) Exited() <-chan struct{} { return s.exit }

// Err is why the process exited. Valid after Exited is closed.
func (s *Sidecar) Err() error { return s.err }

func (s *Sidecar) call(ctx context.Context, method string, params, out any) error {
	return s.conn.call(ctx, method, params, out)
}

// Shutdown asks the sidecar to close every session and exit, and kills it if it doesn't within the
// timeout.
func (s *Sidecar) Shutdown(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = s.conn.call(ctx, "shutdown", struct{}{}, nil)
	_ = s.in.Close() // EOF on its stdin also means shut down
	select {
	case <-s.exit:
	case <-ctx.Done():
		s.Kill()
		<-s.exit
	}
}

// Kill stops the process at once. Its claude processes stop themselves: each one's watcher sees its
// pipe to the sidecar close (see GROUP_WATCH_SCRIPT in sidecar/src/session.ts).
func (s *Sidecar) Kill() {
	if s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
}

// tail keeps the last lines written, for error messages.
type tail struct {
	mu    sync.Mutex
	lines []string
}

func (t *tail) add(line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, line)
	if len(t.lines) > 20 {
		t.lines = t.lines[len(t.lines)-20:]
	}
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// FindCommand locates the sidecar: $SOLO_AGENT, then solo-agent next to the executable, then the
// sidecar sources in a checkout run with bun (for development).
func FindCommand() (Command, error) {
	if p := os.Getenv("SOLO_AGENT"); p != "" {
		if strings.HasSuffix(p, ".ts") {
			return bunCommand(p)
		}
		return Command{Path: p}, nil
	}
	name := "solo-agent"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		for _, p := range []string{filepath.Join(dir, name), filepath.Join(dir, "..", "bin", name)} {
			if _, err := os.Stat(p); err == nil {
				return Command{Path: p}, nil
			}
		}
	}
	// Development: walk up from the working directory to the checkout's sidecar.
	if wd, err := os.Getwd(); err == nil {
		for dir := wd; ; dir = filepath.Dir(dir) {
			src := filepath.Join(dir, "sidecar", "src", "index.ts")
			if _, err := os.Stat(src); err == nil {
				return bunCommand(src)
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return Command{}, errors.New("solo-agent not found: build it with `bun run build` in sidecar/, or set SOLO_AGENT")
}

func bunCommand(src string) (Command, error) {
	bun, err := exec.LookPath("bun")
	if err != nil {
		home, _ := os.UserHomeDir()
		candidate := filepath.Join(home, ".bun", "bin", "bun")
		if _, serr := os.Stat(candidate); serr != nil {
			return Command{}, errors.New("bun not found: install it from https://bun.sh")
		}
		bun = candidate
	}
	return Command{Path: bun, Args: []string{"run", src}}, nil
}
