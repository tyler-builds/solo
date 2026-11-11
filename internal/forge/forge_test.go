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
