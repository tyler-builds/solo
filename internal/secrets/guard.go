//go:build !js

package secrets

import (
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// The guard refuses an agent's tool call only when it would read Solo's secrets: a keyring reader
// pointed at Solo's service, or the environment of a command run_with_secrets is running. Anything
// else that mentions keyrings or /proc, such as searching code for "solo-secrets" or reading an
// unrelated process's environment, goes through. It closes the obvious ways, not every way: a program
// the agent writes can still do either (see docs/ARCHITECTURE.md, Secrets).
//
// Agents' own Bash, PowerShell, Read, Grep and Glob calls are checked through the sidecar's PreToolUse
// hook (sidecar/src/session.ts, the tool.guard request), and commands run with secrets by Forbidden.

// keyringReaders match a program or call that reads a keyring item. A command is refused only when it
// also names Solo's service (go-keyring files every item under it: an attribute on Linux, the target
// name "solo-secrets:NAME" on Windows, the service on macOS).
var keyringReaders = regexp.MustCompile(`(?i)\bsecret-tool\s+(lookup|search)\b|org\.freedesktop\.secrets|\bkwallet-query\b|` +
	`\bkeyring\s+get\b|get_password\s*\(|\bfind-(generic|internet)-password\b|\bcmdkey\b|\bCredRead|` +
	`\bGet-StoredCredential\b|\bPasswordVault\b|\bkeyring\.Get\s*\(`)

var service = regexp.MustCompile(`(?i)solo-secrets`)

// procEnv matches a process's environment (or memory, which holds it) under /proc, with the process.
var procEnv = regexp.MustCompile(`/proc/([^/\s'"]+)/(environ|mem)\b`)

// psEnv matches ps with the BSD "e" option, which prints each process's environment.
var psEnv = regexp.MustCompile(`\bps\s+([a-zA-Z]*e[a-zA-Z]*)\b`)

const refusal = "Solo doesn't let agents read its secrets "

// Guard says why an agent's tool call may not run (it reads Solo's secrets), or "" when it may.
func Guard(tool string, input map[string]any) string {
	switch tool {
	case "Bash", "PowerShell":
		c, _ := input["command"].(string)
		return Forbidden(c)
	case "Read", "Grep", "Glob":
		for _, k := range []string{"file_path", "path"} {
			if p, _ := input[k].(string); p != "" {
				if why := readsRunEnv(p); why != "" {
					return why
				}
			}
		}
	}
	return ""
}

// Forbidden says why a shell command may not run (it reads Solo's secrets), or "" when it may.
func Forbidden(command string) string {
	if m := keyringReaders.FindString(command); m != "" && service.MatchString(command) {
		return refusal + "from the system keyring (" + m + " on solo-secrets). Use run_with_secrets with the " +
			"secret's name: Solo sets it in that one command for you."
	}
	if why := readsRunEnv(command); why != "" {
		return why
	}
	if running.any() && psEnv.MatchString(command) {
		return refusal + "from the environment of a command run with them (ps e) while one runs."
	}
	return ""
}

// readsRunEnv says why text reads the environment of a command running with secrets, or "".
func readsRunEnv(text string) string {
	for _, m := range procEnv.FindAllStringSubmatch(text, -1) {
		if targetsRun(m[1]) {
			return refusal + "from the environment of a command run with them (" + m[0] + ")."
		}
	}
	return ""
}

// targetsRun reports whether a /proc entry may be a process run_with_secrets started: a process in
// one of their groups, or any process (a glob, a variable) while one runs. The caller's own entry
// (self) never is.
func targetsRun(p string) bool {
	switch p {
	case "self", "thread-self", "$$", "${$}":
		return false
	}
	if pid, err := strconv.Atoi(p); err == nil {
		return running.has(pid) || running.has(processGroup(pid))
	}
	return running.any()
}

// processGroup is a process's group ID on Linux, from /proc; -1 elsewhere or when it is gone.
func processGroup(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return -1
	}
	// pid (comm) state ppid pgrp …; comm may hold spaces and parentheses.
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return -1
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 3 {
		return -1
	}
	g, err := strconv.Atoi(f[2])
	if err != nil {
		return -1
	}
	return g
}

// groups are the process groups of the commands running with secrets (their leaders' PIDs, which are
// the group IDs on POSIX).
type groups struct {
	mu sync.Mutex
	m  map[int]int
}

var running groups

func (g *groups) add(pid int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[int]int{}
	}
	g.m[pid]++
}

func (g *groups) remove(pid int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m[pid]--; g.m[pid] <= 0 {
		delete(g.m, pid)
	}
}

func (g *groups) has(pid int) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return pid > 0 && g.m[pid] > 0
}

func (g *groups) any() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.m) > 0
}
