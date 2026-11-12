package api

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tyler-builds/solo/internal/core"
)

func TestSettingsPatchRoundTrip(t *testing.T) {
	old := core.DefaultSettings()
	old.Folders = []string{"/a", "/b"}
	old.ShowCosts = true
	old.RepoBranchRules = map[string]string{"/a": "x"}
	old.BranchRules = "keep"

	next := old
	next.Folders = []string{"/a"}
	next.ShowCosts = false
	next.WorkerModel = "opus"
	next.RepoBranchRules = nil
	next.SoloSessionTokens = -1

	p := DiffSettings(old, next)
	for _, name := range []string{"folders", "showCosts", "workerModel", "repoBranchRules", "soloSessionTokens"} {
		if _, ok := p[name]; !ok {
			t.Errorf("patch lacks %q: %v", name, p)
		}
	}
	if len(p) != 5 {
		t.Errorf("patch has %d fields, want 5: %v", len(p), p)
	}
	got := old
	if err := p.Apply(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, next) {
		t.Errorf("applied:\n got %+v\nwant %+v", got, next)
	}
	// A patch changes only its own fields: another device's change to a different one survives.
	other := old
	other.CommitRules = "from another device"
	if err := p.Apply(&other); err != nil {
		t.Fatal(err)
	}
	if other.CommitRules != "from another device" || other.WorkerModel != "opus" {
		t.Errorf("the patch undid a different change: %+v", other)
	}
	if len(DiffSettings(old, old)) != 0 {
		t.Error("equal settings differ")
	}
}

func TestSettingsPatchEveryField(t *testing.T) {
	// Each field alone makes a patch of one entry that applies back: a field added to core.Settings
	// with a type the patch can't carry fails here.
	typ := reflect.TypeOf(core.Settings{})
	for i := 0; i < typ.NumField(); i++ {
		var next core.Settings
		f := reflect.ValueOf(&next).Elem().Field(i)
		switch f.Kind() {
		case reflect.String:
			f.SetString("v")
		case reflect.Int:
			f.SetInt(3)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Slice:
			f.Set(reflect.ValueOf([]string{"v"}))
		case reflect.Map:
			f.Set(reflect.ValueOf(map[string]string{"k": "v"}))
		default:
			t.Fatalf("%s: a kind of setting the patch doesn't handle: %s", typ.Field(i).Name, f.Kind())
		}
		p := DiffSettings(core.Settings{}, next)
		if len(p) != 1 {
			t.Fatalf("%s: patch %v", typ.Field(i).Name, p)
		}
		var got core.Settings
		if err := p.Apply(&got); err != nil || !reflect.DeepEqual(got, next) {
			t.Fatalf("%s: applied %+v, %v", typ.Field(i).Name, got, err)
		}
		back := DiffSettings(next, core.Settings{})
		if err := back.Apply(&got); err != nil || !reflect.DeepEqual(got, core.Settings{}) {
			t.Fatalf("%s: cleared %+v, %v", typ.Field(i).Name, got, err)
		}
	}
}

func TestSettingsPatchApplyRejects(t *testing.T) {
	for name, p := range map[string]SettingsPatch{
		"unknown field": {"nope": []byte(`1`)},
		"wrong type":    {"scanDepth": []byte(`"3"`)},
		"two values":    {"scanDepth": []byte(`3 4`)},
		"object":        {"folders": []byte(`{"a":1}`)},
	} {
		s := core.DefaultSettings()
		before := s
		if err := p.Apply(&s); err == nil {
			t.Errorf("%s: applied", name)
		}
		if !reflect.DeepEqual(s, before) {
			t.Errorf("%s: a failed patch changed the settings", name)
		}
	}
}

func TestSettingsPatchCheckRemote(t *testing.T) {
	isDir := func(p string) bool { return p == "/code" }
	ok := SettingsPatch{"folders": []byte(`["/code"]`), "scanDepth": []byte(`4`), "workerModel": []byte(`"opus"`),
		"mergeMethod": []byte(`"rebase"`), "showCosts": null}
	if err := ok.CheckRemote(isDir); err != nil {
		t.Errorf("a valid patch: %v", err)
	}
	for name, p := range map[string]SettingsPatch{
		"claude path":   {"claudePath": []byte(`"/tmp/evil"`)},
		"clear claude":  {"claudePath": null},
		"editor":        {"editorCommand": []byte(`"sh -c x"`)},
		"terminal":      {"terminalCommand": []byte(`"sh -c x"`)},
		"missing dir":   {"folders": []byte(`["/nowhere"]`)},
		"depth":         {"scanDepth": []byte(`40`)},
		"zero depth":    {"scanDepth": []byte(`0`)},
		"merge method":  {"mergeMethod": []byte(`"force"`)},
		"checks":        {"onChecksFail": []byte(`"explode"`)},
		"model newline": {"workerModel": []byte(`"a\nb"`)},
		"long rules":    {"prRules": []byte(`"` + strings.Repeat("x", maxSettingText+1) + `"`)},
		"long commits":  {"commitRules": []byte(`"` + strings.Repeat("x", maxSettingText+1) + `"`)},
		"long titles":   {"prTitleRules": []byte(`"` + strings.Repeat("x", maxSettingText+1) + `"`)},
		"long descs":    {"prDescriptionRules": []byte(`"` + strings.Repeat("x", maxSettingText+1) + `"`)},
		"unknown":       {"x": []byte(`1`)},
		"huge":          {"branchRules": []byte(`"` + strings.Repeat("x", maxPatchBytes) + `"`)},
	} {
		if err := p.CheckRemote(isDir); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	for name := range settingFields {
		if want := name == "claudePath" || name == "editorCommand" || name == "terminalCommand"; DesktopOnly(name) != want {
			t.Errorf("DesktopOnly(%q) = %v", name, !want)
		}
		// Every setting is listed, one way or the other: a new one must be decided, not defaulted.
		if _, listed := remote[name]; !listed {
			t.Errorf("the setting %q isn't listed as remote or not (api.remote)", name)
		}
	}
	if !DesktopOnly("somethingNew") {
		t.Error("a setting nobody listed can be changed remotely")
	}
}
