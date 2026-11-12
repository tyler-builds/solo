//go:build !js

package core

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Where Claude Code keeps skills and commands, as read here:
//
//	~/.claude/skills/<name>/SKILL.md                          the user's own skills
//	~/.claude/commands/<name>.md, <folder>/<name>.md          the user's commands ("folder:name")
//	~/.claude/skills/synced/<bucket>/<name>/SKILL.md          skills synced from the user's organization,
//	                                                          invoked as "anthropic-skills:<name>"
//	~/.claude/plugins/synced/<bucket>/<plugin>/               plugins synced from the organization
//	~/.claude/plugins/installed_plugins.json                  plugins installed from a marketplace: each
//	                                                          entry's installPath holds the plugin
//	<plugin>/skills/<name>/SKILL.md, <plugin>/commands/*.md   a plugin's, invoked as "<plugin>:<name>"
//	<project>/.claude/skills/<name>/SKILL.md, .../commands    a project's
//
// ~/.claude is CLAUDE_CONFIG_DIR when that is set, as for Claude Code.

// Limits on what is read, so a folder full of files can't slow Solo down.
const (
	maxSkillEntries  = 500      // skills and commands listed
	maxSkillHead     = 16 << 10 // bytes read of a file for its frontmatter
	maxSkillFile     = 64 << 10 // bytes read of a skill for its instructions
	maxSkillRules    = 8 << 10  // bytes of one skill's instructions given to the PR text model
	maxCommandDepth  = 3        // folders under commands/
	syncedSkillsName = "anthropic-skills"
)

// claudeDir is Claude Code's folder of settings, skills and plugins.
func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude")
}

// skillFile is a skill or command found on disk.
type skillFile struct {
	Skill
	path string // SKILL.md, or the command's .md
}

// findSkills lists the skills and commands a Claude session with project as its working folder ("" for
// none) can invoke. When two share a name, the user's own come before the project's, as in Claude Code.
func findSkills(project string) []skillFile {
	var out []skillFile
	seen := map[string]bool{}
	add := func(fs []skillFile) {
		for _, f := range fs {
			key := f.Name
			if f.Command {
				key = "command " + key
			}
			if seen[key] || len(out) >= maxSkillEntries {
				continue
			}
			seen[key] = true
			out = append(out, f)
		}
	}
	dir := claudeDir()
	if dir != "" {
		skills := filepath.Join(dir, "skills")
		add(skillsIn(skills, "", "Personal", "synced"))
		add(commandsIn(filepath.Join(dir, "commands"), "", "Personal"))
		if !isFile(filepath.Join(skills, "synced", "SKILL.md")) {
			for _, bucket := range subdirs(filepath.Join(skills, "synced")) {
				add(skillsIn(bucket, syncedSkillsName+":", "Organization", ""))
			}
		}
	}
	if project != "" {
		add(skillsIn(filepath.Join(project, ".claude", "skills"), "", "Project", ""))
		add(commandsIn(filepath.Join(project, ".claude", "commands"), "", "Project"))
	}
	for _, p := range pluginDirs(dir, project) {
		name := pluginName(p.dir, p.id)
		add(skillsIn(filepath.Join(p.dir, "skills"), name+":", "Plugin "+name, ""))
		add(commandsIn(filepath.Join(p.dir, "commands"), name+":", "Plugin "+name))
	}
	return out
}

// skillsIn lists the skills in a skills folder: each subfolder with a SKILL.md, named by its
// frontmatter's name or else the folder's, after prefix. skip is a subfolder that isn't a skill.
func skillsIn(dir, prefix, source, skip string) []skillFile {
	var out []skillFile
	for _, d := range subdirs(dir) {
		if filepath.Base(d) == skip && !isFile(filepath.Join(d, "SKILL.md")) {
			continue
		}
		path := filepath.Join(d, "SKILL.md")
		front, _, err := readMarkdown(path, maxSkillHead)
		if err != nil {
			continue
		}
		name := cmp.Or(cleanName(frontmatterField(front, "name")), filepath.Base(d))
		out = append(out, skillFile{Skill{Name: prefix + name, Description: frontmatterField(front, "description"), Source: source}, path})
	}
	return out
}

// commandsIn lists the commands in a commands folder: each .md file, named by its path with ":" for
// each folder, after prefix.
func commandsIn(dir, prefix, source string) []skillFile {
	var out []skillFile
	var walk func(d, ns string, depth int)
	walk = func(d, ns string, depth int) {
		entries, err := os.ReadDir(d)
		if err != nil {
			return
		}
		for _, e := range entries {
			n := e.Name()
			if strings.HasPrefix(n, ".") || len(out) >= maxSkillEntries {
				continue
			}
			p := filepath.Join(d, n)
			if e.IsDir() || e.Type()&os.ModeSymlink != 0 && isDir(p) {
				if depth < maxCommandDepth {
					walk(p, ns+n+":", depth+1)
				}
				continue
			}
			base, ok := strings.CutSuffix(n, ".md")
			if !ok || base == "" {
				continue
			}
			front, body, err := readMarkdown(p, maxSkillHead)
			if err != nil {
				continue
			}
			desc := cmp.Or(frontmatterField(front, "description"), firstTextLine(body))
			out = append(out, skillFile{Skill{Name: prefix + ns + base, Description: clipText(desc, 300), Command: true, Source: source}, p})
		}
	}
	walk(dir, "", 0)
	return out
}

// plugin is an installed plugin's folder, and its ID ("name@marketplace") when it has one.
type plugin struct{ dir, id string }

// pluginDirs are the plugins installed for a session in project: the organization's synced plugins
// and those in installed_plugins.json (installed for the user, or for project), less any that
// settings.json turns off.
func pluginDirs(claude, project string) []plugin {
	if claude == "" {
		return nil
	}
	var out []plugin
	for _, bucket := range subdirs(filepath.Join(claude, "plugins", "synced")) {
		for _, d := range subdirs(bucket) {
			out = append(out, plugin{dir: d})
		}
	}
	off := disabledPlugins(claude, project)
	data, err := os.ReadFile(filepath.Join(claude, "plugins", "installed_plugins.json"))
	if err != nil {
		return out
	}
	var f struct {
		Plugins map[string]json.RawMessage `json:"plugins"`
	}
	if json.Unmarshal(data, &f) != nil {
		return out
	}
	type install struct {
		InstallPath string `json:"installPath"`
		Scope       string `json:"scope"`
		ProjectPath string `json:"projectPath"`
	}
	for _, id := range slices.Sorted(func(yield func(string) bool) {
		for k := range f.Plugins {
			if !yield(k) {
				return
			}
		}
	}) {
		if off[id] {
			continue
		}
		var list []install // version 2: a list of installs; version 1: one
		if json.Unmarshal(f.Plugins[id], &list) != nil {
			var one install
			if json.Unmarshal(f.Plugins[id], &one) != nil {
				continue
			}
			list = []install{one}
		}
		for _, in := range list {
			if in.InstallPath == "" || (in.ProjectPath != "" && (project == "" || filepath.Clean(in.ProjectPath) != filepath.Clean(project))) {
				continue
			}
			if isDir(in.InstallPath) && !slices.ContainsFunc(out, func(p plugin) bool { return p.dir == in.InstallPath }) {
				out = append(out, plugin{in.InstallPath, id})
			}
		}
	}
	return out
}

// disabledPlugins are the plugins ("name@marketplace") that the user's or the project's settings turn
// off with enabledPlugins.
func disabledPlugins(claude, project string) map[string]bool {
	off := map[string]bool{}
	files := []string{filepath.Join(claude, "settings.json")}
	if project != "" {
		files = append(files, filepath.Join(project, ".claude", "settings.json"), filepath.Join(project, ".claude", "settings.local.json"))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s struct {
			EnabledPlugins map[string]any `json:"enabledPlugins"`
		}
		if json.Unmarshal(data, &s) != nil {
			continue
		}
		for id, v := range s.EnabledPlugins {
			if on, ok := v.(bool); ok {
				off[id] = !on
			}
		}
	}
	return off
}

// pluginName is a plugin's name: its manifest's, or else its ID's before the "@", or its folder's.
func pluginName(dir, id string) string {
	if data, err := os.ReadFile(filepath.Join(dir, ".claude-plugin", "plugin.json")); err == nil {
		var m struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(data, &m) == nil {
			if n := cleanName(m.Name); n != "" {
				return n
			}
		}
	}
	if n, _, _ := strings.Cut(id, "@"); cleanName(n) != "" {
		return n
	}
	return filepath.Base(dir)
}

// cleanName is a name from a file, or "" when it can't be one: Claude Code's names are short, with no
// spaces or slashes.
func cleanName(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 100 || strings.ContainsAny(s, " \t\r\n/\\\"'`") {
		return ""
	}
	return s
}

// subdirs lists a folder's subfolders (following links), without hidden ones, in name order.
func subdirs(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if e.IsDir() || e.Type()&os.ModeSymlink != 0 && isDir(p) {
			out = append(out, p)
		}
	}
	return out
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// readMarkdown reads at most limit bytes of a Markdown file, split into frontmatter and body.
func readMarkdown(path string, limit int64) (front, body string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		return "", "", fmt.Errorf("%s isn't a file", path)
	}
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", "", err
	}
	front, body = splitFrontmatter(string(data))
	return front, body, nil
}

// skillRules reads the instructions of the skills named, as found for a session in project, for the
// PR text model, which can't run tools to load them itself: each skill's SKILL.md without its
// frontmatter, cut at maxSkillRules. missing lists the names not found.
func skillRules(project string, names []string) (rules []namedText, missing []string) {
	if len(names) == 0 {
		return nil, nil
	}
	found := map[string]string{}
	for _, f := range findSkills(project) {
		if !f.Command {
			found[f.Name] = f.path
		}
	}
	for _, n := range names {
		path, ok := found[n]
		if !ok {
			missing = append(missing, n)
			continue
		}
		_, body, err := readMarkdown(path, maxSkillFile)
		if err != nil {
			missing = append(missing, n)
			continue
		}
		body = strings.TrimSpace(body)
		if len(body) > maxSkillRules {
			body = strings.TrimSpace(clipText(body, maxSkillRules)) + "\n(The rest of this skill is cut.)"
		}
		if body != "" {
			rules = append(rules, namedText{n, body})
		}
	}
	return rules, missing
}

// namedText is a text and what it is called.
type namedText struct{ name, text string }

// Skills lists the skills and commands the conversation threadID can invoke with "/": the user's,
// their organization's, installed plugins', and those of the thread's project. Solo's own conversation
// has no project. With threadID "", it lists the skills (not commands) Settings offers for pull request
// text: those and every known repository's, each repository's marked with its name.
func (c *Core) Skills(ctx context.Context, threadID string) ([]Skill, error) {
	if threadID == "" {
		var out []Skill
		seen := map[string]bool{}
		for _, f := range findSkills("") {
			if !f.Command && !seen[f.Name] {
				seen[f.Name] = true
				out = append(out, f.Skill)
			}
		}
		for _, repo := range c.RepoPaths() {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			for _, f := range skillsIn(filepath.Join(repo, ".claude", "skills"), "", "Project "+filepath.Base(repo), "") {
				if !seen[f.Name] {
					seen[f.Name] = true
					out = append(out, f.Skill)
				}
			}
		}
		sortSkills(out)
		return out, nil
	}
	project := ""
	if threadID != OrchestratorID {
		c.mu.Lock()
		t := c.threads[threadID]
		if t != nil {
			project = c.sessionDir(t.row)
		}
		c.mu.Unlock()
		if t == nil {
			return nil, fmt.Errorf("no thread %s", threadID)
		}
	}
	fs := findSkills(project)
	out := make([]Skill, len(fs))
	for i, f := range fs {
		out[i] = f.Skill
	}
	sortSkills(out)
	return out, nil
}

func sortSkills(s []Skill) {
	slices.SortStableFunc(s, func(a, b Skill) int { return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)) })
}
