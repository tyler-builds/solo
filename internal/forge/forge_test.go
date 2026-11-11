package forge

import (
	"encoding/json"
	"testing"
)

func TestTallyChecks(t *testing.T) {
	// gh pr view --json statusCheckRollup, trimmed: check runs in each state and a commit status.
	const rollup = `[
		{"__typename":"CheckRun","name":"build","status":"COMPLETED","conclusion":"SUCCESS"},
		{"__typename":"CheckRun","name":"lint","status":"COMPLETED","conclusion":"SKIPPED"},
		{"__typename":"CheckRun","name":"test","status":"IN_PROGRESS","conclusion":""},
		{"__typename":"CheckRun","name":"e2e","status":"QUEUED","conclusion":""},
		{"__typename":"CheckRun","name":"vet","status":"COMPLETED","conclusion":"FAILURE","detailsUrl":"https://example.com/runs/1"},
		{"__typename":"StatusContext","context":"ci/deploy-preview","state":"SUCCESS"},
		{"__typename":"StatusContext","context":"ci/docs","state":"PENDING"}
	]`
	var rs []rollupEntry
	if err := json.Unmarshal([]byte(rollup), &rs); err != nil {
		t.Fatal(err)
	}
	failed, got := tallyChecks(rs)
	want := CheckCounts{Total: 7, Passed: 3, Failed: 1, Pending: 3}
	if got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
	if len(failed) != 1 || failed[0].Name != "vet" || failed[0].URL != "https://example.com/runs/1" {
		t.Errorf("failed = %+v, want vet with its link", failed)
	}
	if _, c := tallyChecks(nil); c != (CheckCounts{}) {
		t.Errorf("no checks counted %+v", c)
	}
}

func TestFindURLsAndSameRepo(t *testing.T) {
	out := "Creating pull request for perf/text-draw-cache into main in acme/jamb\n\nhttps://github.com/acme/jamb/pull/93\n"
	if got := FindURLs(out + out); len(got) != 1 || got[0] != "https://github.com/acme/jamb/pull/93" || Number(got[0]) != 93 {
		t.Fatalf("FindURLs %v", got)
	}
	for _, c := range []struct {
		pr, remote string
		want       bool
	}{
		{"https://github.com/acme/jamb/pull/93", "git@github.com:acme/jamb.git", true},
		{"https://github.com/acme/jamb/pull/93", "https://github.com/Acme/jamb", true},
		{"https://github.com/acme/jamb/pull/93", "ssh://git@github.com/acme/jamb.git", true},
		{"https://github.com/acme/jamb/pull/93", "git@github.com:acme/other.git", false},
		{"https://github.com/upstream/jamb/pull/93", "git@github.com:acme/jamb.git", false},
		{"https://gitlab.com/g/sub/proj/-/merge_requests/4", "git@gitlab.com:g/sub/proj.git", true},
		{"https://github.com/acme/jamb/pull/93", "/srv/git/jamb.git", false},
	} {
		if got := SameRepo(c.pr, c.remote); got != c.want {
			t.Errorf("SameRepo(%s, %s) = %v", c.pr, c.remote, got)
		}
	}
	if KnownRemote("/srv/git/jamb.git") || !KnownRemote("git@github.com:acme/jamb.git") {
		t.Fatal("KnownRemote")
	}
}
