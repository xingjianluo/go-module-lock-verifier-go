package verify

import (
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// TestModuleSetDrift covers the report statuses unlocked / extra-locked /
// version-mismatch, each of which is a lock_stale finding but must be
// distinguishable in the diff report.
func TestModuleSetDrift(t *testing.T) {
	f := newFixture(t)
	basicGraph(t, f.cache)
	mf := f.writeManifest("module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/a v1.2.3\n" +
		"require example.com/b v0.1.0\n")
	f.lockNow(mf)

	// Add a brand new direct dependency cached locally -> unlocked module.
	f.cache.Add(t, testutilModNew())
	changed := "module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/a v1.2.3\n" +
		"require example.com/b v0.1.0\n" +
		"require example.com/new v0.9.0\n"
	testutilWrite(f, f.modPath, changed)

	res, errs, err := Run(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if !lockKind(errs, model.LockStale) {
		t.Fatalf("module set change must be lock_stale, got %v", errs)
	}
	status := map[string]model.DiffEntry{}
	for _, e := range res.Report.Entries {
		status[e.Path] = e
	}
	if status["example.com/new"].Status != "unlocked" {
		t.Fatalf("new module must report unlocked, got %q", status["example.com/new"].Status)
	}
	for p, e := range status {
		t.Logf("JUDGEMENT %s -> %s (locked=%s actual=%s)", p, e.Status, e.LockedVer, e.ActualVer)
	}
}
