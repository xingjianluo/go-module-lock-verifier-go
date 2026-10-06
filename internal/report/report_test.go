package report

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

func sampleErrors() []error {
	return []error{
		&model.VerifyError{Kind: model.VerifyHashMismatch, Module: "example.com/z",
			Expected: "sha256:aaa", Actual: "sha256:bbb", IntroducedBy: []string{"example.com/main"}},
		&model.LockError{Kind: model.LockStale, Module: "example.com/a",
			Expected: "v1.0.0", Actual: "v1.1.0", Detail: "locked version differs"},
		&model.RejectError{Kind: model.RejectMalformedVersion, Line: 4, Directive: "require",
			Detail: "bad version"},
		&model.WorkspaceError{Kind: model.WorkspaceRequirementConflict, Module: "example.com/dep",
			Expected: "v1.0.0", Actual: "v1.1.0",
			Sources: []string{"example.com/b: require example.com/dep v1.1.0",
				"example.com/a: require example.com/dep v1.0.0"}},
	}
}

// TestClassifyCategories ensures every model error maps to its documented
// category and carries kind/module/expected/actual through.
func TestClassifyCategories(t *testing.T) {
	issues := Classify(sampleErrors())
	wantCategories := []string{CategoryCache, CategoryLock, CategoryManifest, CategoryWorkspace}
	if len(issues) != len(wantCategories) {
		t.Fatalf("got %d issues, want %d", len(issues), len(wantCategories))
	}
	for i, want := range wantCategories {
		if issues[i].Category != want {
			t.Fatalf("issue %d category = %q, want %q (sort order broken)", i, issues[i].Category, want)
		}
		t.Logf("ISSUE %d category=%s kind=%s module=%s expected=%s actual=%s sources=%v",
			i, issues[i].Category, issues[i].Kind, issues[i].Module,
			issues[i].Expected, issues[i].Actual, issues[i].Sources)
	}
	// The workspace issue must keep its introducer list for CI branching.
	ws := issues[3]
	if len(ws.Sources) != 2 || !strings.Contains(ws.Sources[0], "example.com/a") {
		t.Fatalf("workspace issue lost sources: %+v", ws)
	}
	t.Logf("JUDGEMENT categories sorted %v", wantCategories)
}

// TestMarshalDeterministic marshals the same result repeatedly and requires
// byte-identical output with stable field order. Module entries are built
// through ModulesFromReport from a deliberately unsorted diff report to
// prove entry ordering does not depend on producer order.
func TestMarshalDeterministic(t *testing.T) {
	rep := &model.DiffReport{ModulePath: "example.com/ws", Entries: []model.DiffEntry{
		{Path: "example.com/b", Status: "ok", LockedVer: "v0.1.0", ActualVer: "v0.1.0"},
		{Path: "example.com/a", Status: "hash-mismatch", LockedHash: "sha256:1", ActualHash: "sha256:2"},
	}}
	res := &Result{
		Command: "audit", OK: false, Subject: "example.com/ws",
		LockPath: "go.lock",
		Modules:  ModulesFromReport(rep),
		Issues:   Classify(sampleErrors()),
	}
	first, err := Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		again, err := Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("marshal %d differs", i)
		}
	}
	// Module entries must be path-sorted regardless of input order.
	var decoded map[string]any
	if err := json.Unmarshal(first, &decoded); err != nil {
		t.Fatal(err)
	}
	text := string(first)
	if strings.Index(text, "example.com/a") > strings.Index(text, "example.com/b") {
		t.Fatal("module entries must be sorted by path")
	}
	// Field order: command before ok before modules before issues.
	if strings.Index(text, "\"command\"") > strings.Index(text, "\"ok\"") ||
		strings.Index(text, "\"ok\"") > strings.Index(text, "\"modules\"") {
		t.Fatal("top-level field order is not stable")
	}
	t.Logf("JSON OUTPUT:\n%s", first)
	t.Log("JUDGEMENT 5 repeated marshals byte-identical, fields and entries ordered")
}
