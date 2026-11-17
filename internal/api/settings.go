package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/tyler-builds/solo/internal/core"
)

// SettingsPatch is a change to the settings: the new value of each field that changed, by the field's
// JSON name, with null for a field set back to its zero value. A remote client sends the change it made
// rather than the settings it has, so two devices changing different settings don't undo each other, and
// the machine can check each field (see CheckRemote).
type SettingsPatch map[string]json.RawMessage

var null = json.RawMessage("null")

// Limits on what a patch from a remote device may hold.
const (
	maxPatchBytes   = 256 << 10
	maxSettingText  = 16 << 10 // rules written for a model
	maxSettingName  = 256      // a model, an effort, a method
	maxSettingPath  = 4096
	maxFolders      = 64
	maxRepoRules    = 256
	maxPRSkills     = 32 // skills followed for pull request text
	minScanDepth    = 1
	maxScanDepth    = 6
	minSessionLimit = -1
	maxSessionLimit = 10_000_000
)

// remote says, for every setting by its JSON name, whether a remote device may change it. The ones it
// may not are the settings that name programs Solo runs: changing one remotely would run a program of
// the sender's choosing on the machine, so they are set at the machine only. A setting that is not
// listed here can't be changed remotely either, and a test fails until it is: a new setting is never
// remote by default.
var remote = map[string]bool{
	"claudePath":      false,
	"editorCommand":   false,
	"terminalCommand": false,

	"folders": true, "scanDepth": true, "orchestratorModel": true, "workerModel": true, "showCosts": true,
	"noNotifications": true, "noResumeAfterRestart": true, "soloSessionTokens": true, "orchestratorEffort": true,
	"workerEffort": true, "namingModel": true, "namingEffort": true, "branchRules": true, "repoBranchRules": true,
	"prModel": true, "prEffort": true, "prRules": true, "skipPRDescription": true, "autoMerge": true,
	"mergeMethod": true, "onChecksFail": true,
	"setup": false, "hints": false, // the first-run setup and its hints run at the machine
	"commitRules": true, "prTitleRules": true, "prDescriptionRules": true, "prSkills": true,
}

// DesktopOnly reports whether the setting with this JSON name can be changed at the machine only.
func DesktopOnly(name string) bool { return !remote[name] }

// settingFields maps each setting's JSON name to its field's index.
var settingFields = func() map[string]int {
	out := map[string]int{}
	t := reflect.TypeOf(core.Settings{})
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name == "" || name == "-" {
			panic("api: setting " + t.Field(i).Name + " has no JSON name")
		}
		out[name] = i
	}
	return out
}()

// DiffSettings returns the patch that turns old into new.
func DiffSettings(old, new core.Settings) SettingsPatch {
	p := SettingsPatch{}
	a, b := reflect.ValueOf(old), reflect.ValueOf(new)
	for name, i := range settingFields {
		fa, fb := a.Field(i), b.Field(i)
		if reflect.DeepEqual(fa.Interface(), fb.Interface()) || (empty(fa) && empty(fb)) {
			continue
		}
		if empty(fb) {
			p[name] = null
			continue
		}
		data, err := json.Marshal(fb.Interface())
		if err != nil {
			panic(err) // settings are strings, numbers, lists and maps of strings
		}
		p[name] = data
	}
	return p
}

// empty reports whether a field holds nothing: its zero value, or a list or map with no entries.
func empty(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Slice, reflect.Map:
		return v.Len() == 0
	}
	return v.IsZero()
}

// Apply makes the patch's changes to s. It fails, changing nothing, on a field it doesn't know or a
// value of the wrong type.
func (p SettingsPatch) Apply(s *core.Settings) error {
	next := *s
	v := reflect.ValueOf(&next).Elem()
	for _, name := range p.names() {
		i, ok := settingFields[name]
		if !ok {
			return fmt.Errorf("unknown setting %q", name)
		}
		f := v.Field(i)
		raw := p[name]
		if bytes.Equal(bytes.TrimSpace(raw), null) {
			f.SetZero()
			continue
		}
		fresh := reflect.New(f.Type())
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(fresh.Interface()); err != nil {
			return fmt.Errorf("setting %q: %w", name, err)
		}
		if dec.More() {
			return fmt.Errorf("setting %q: more than one value", name)
		}
		f.Set(fresh.Elem())
	}
	*s = next
	return nil
}

func (p SettingsPatch) names() []string {
	out := make([]string, 0, len(p))
	for name := range p {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CheckRemote reports why a patch from a remote device must not be applied: it changes a setting that
// is the machine's only, or holds a value outside the limits. isDir reports whether a path is a folder
// on the machine, for the folders searched for repositories.
func (p SettingsPatch) CheckRemote(isDir func(path string) bool) error {
	size := 0
	for name, raw := range p {
		size += len(name) + len(raw)
		if _, known := settingFields[name]; known && !remote[name] {
			return fmt.Errorf("%q can be changed at the machine only", name)
		}
	}
	if size > maxPatchBytes {
		return errors.New("the settings change is too large")
	}
	var s core.Settings
	if err := p.Apply(&s); err != nil {
		return err
	}
	if _, ok := p["scanDepth"]; ok && (s.ScanDepth < minScanDepth || s.ScanDepth > maxScanDepth) {
		return fmt.Errorf("the search depth is %d to %d", minScanDepth, maxScanDepth)
	}
	if s.SoloSessionTokens < minSessionLimit || s.SoloSessionTokens > maxSessionLimit {
		return errors.New("the session size is out of range")
	}
	if len(s.Folders) > maxFolders {
		return fmt.Errorf("at most %d folders", maxFolders)
	}
	for _, f := range s.Folders {
		if len(f) > maxSettingPath || !isDir(f) {
			return fmt.Errorf("%q isn't a folder on this machine", clip(f))
		}
	}
	if len(s.RepoBranchRules) > maxRepoRules {
		return fmt.Errorf("branch rules for at most %d repositories", maxRepoRules)
	}
	for repo, rules := range s.RepoBranchRules {
		if len(repo) > maxSettingPath || len(rules) > maxSettingText {
			return errors.New("a repository's branch rules are too long")
		}
	}
	for name, v := range map[string]string{
		"branchRules": s.BranchRules, "commitRules": s.CommitRules, "prTitleRules": s.PRTitleRules,
		"prDescriptionRules": s.PRDescriptionRules, "prRules": s.PRRules,
	} {
		if len(v) > maxSettingText {
			return fmt.Errorf("%q is too long", name)
		}
	}
	if len(s.PRSkills) > maxPRSkills {
		return fmt.Errorf("at most %d skills for pull request text", maxPRSkills)
	}
	for _, n := range s.PRSkills {
		if len(n) > maxSettingName || strings.ContainsAny(n, "\x00\r\n") {
			return errors.New("a skill's name isn't valid")
		}
	}
	for name, v := range map[string]string{
		"orchestratorModel": s.OrchestratorModel, "workerModel": s.WorkerModel, "namingModel": s.NamingModel,
		"prModel": s.PRModel, "orchestratorEffort": s.OrchestratorEffort, "workerEffort": s.WorkerEffort,
		"namingEffort": s.NamingEffort, "prEffort": s.PREffort,
	} {
		if len(v) > maxSettingName || strings.ContainsAny(v, "\x00\r\n") {
			return fmt.Errorf("%q isn't a valid value", name)
		}
	}
	if v := s.MergeMethod; v != "" && v != "squash" && v != "merge" && v != "rebase" {
		return fmt.Errorf("the merge method is squash, merge or rebase")
	}
	if v := s.OnChecksFail; v != "" && v != core.ChecksFix && v != core.ChecksNotify {
		return fmt.Errorf("unknown choice for failed checks")
	}
	return nil
}

// clip shortens text shown in an error.
func clip(s string) string {
	if len(s) > 80 {
		return s[:80] + "…"
	}
	return s
}
