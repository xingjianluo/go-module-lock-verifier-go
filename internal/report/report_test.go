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
			Expected: "v1.0.0", Actual: "v1.1.0"},
		&model.RejectError{Kind: model.RejectMalformedVersion, Line: 3, Directive: "require",
			Detail: "bad version"},
		&model.WorkspaceError{Kind: model.WorkspaceReplaceConflict, Subject: "example.com/dep@",
			Entries: []model.WorkspaceConflictEntry{
				{Module: "example.com/b", Dir: "./b", Rule: "x => y"},
				{Module: "example.com/a", Dir: "./a", Rule: "x => z"},
			}},
	}
}

func TestFromErrorsClassifiesAndSorts(t *testing.T) {
	fs := FromErrors(sampleErrors())
	if len(fs) != 4 {
		t.Fatalf("got %d findings", len(fs))
	}
	for _, f := range fs {
		t.Logf("finding: category=%s kind=%s module=%s expected=%s actual=%s introducedBy=%v",
			f.Category, f.Kind, f.Module, f.Expected, f.Actual, f.IntroducedBy)
	}
	// Sorted by category: cache < lock < manifest < workspace.
	wantOrder := []string{CategoryCache, CategoryLock, CategoryManifest, CategoryWorkspace}
	for i, want := range wantOrder {
		if fs[i].Category != want {
			t.Fatalf("finding %d category = %s, want %s", i, fs[i].Category, want)
		}
	}
	if fs[0].Kind != string(model.VerifyHashMismatch) || fs[0].Expected != "sha256:aaa" {
		t.Fatalf("cache finding lost data: %+v", fs[0])
	}
	if len(fs[0].IntroducedBy) != 1 || fs[0].IntroducedBy[0] != "example.com/main" {
		t.Fatalf("cache finding lost introducedBy: %+v", fs[0])
	}
	ws := fs[3]
	if ws.Subject != "example.com/dep@" || len(ws.IntroducedBy) != 2 {
		t.Fatalf("workspace conflict lost contributors: %+v", ws)
	}
}

func TestMarshalIsDeterministic(t *testing.T) {
	build := func() *Report {
		return &Report{
			SchemaVersion: SchemaVersion, Command: "audit", OK: false,
			Subject: "example.com/ws", Modules: []string{"example.com/a", "example.com/b"},
			LockedModules: 3,
			Findings:      FromErrors(sampleErrors()),
		}
	}
	d1, err := Marshal(build())
	if err != nil {
		t.Fatal(err)
	}
	d2, err := Marshal(build())
	if err != nil {
		t.Fatal(err)
	}
	if string(d1) != string(d2) {
		t.Fatal("same report marshaled differently")
	}
	// Field order is fixed: schema_version first, findings last.
	s := string(d1)
	t.Logf("JSON:\n%s", s)
	if !strings.HasPrefix(s, "{\n  \"schema_version\": \"1\",\n  \"command\": \"audit\"") {
		t.Fatalf("unexpected field order:\n%s", s[:120])
	}
	// Output is valid JSON with the expected shape.
	var decoded struct {
		OK       bool `json:"ok"`
		Findings []struct {
			Category string `json:"category"`
			Kind     string `json:"kind"`
		} `json:"findings"`
	}
	if err := json.Unmarshal(d1, &decoded); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if decoded.OK || len(decoded.Findings) != 4 {
		t.Fatalf("decoded %+v", decoded)
	}
}

func TestMarshalEmptyFindingsIsArrayNotNull(t *testing.T) {
	data, err := Marshal(&Report{SchemaVersion: SchemaVersion, Command: "lock", OK: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"findings": []`) {
		t.Fatalf("findings must be a stable empty array:\n%s", data)
	}
}
