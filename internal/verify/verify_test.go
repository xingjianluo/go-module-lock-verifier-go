package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/resolver"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

type fixture struct {
	t        *testing.T
	cache    *testutil.Cache
	dir      string
	modPath  string
	lockPath string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	c := testutil.NewCache(t)
	dir := t.TempDir()
	return &fixture{
		t: t, cache: c, dir: dir,
		modPath:  filepath.Join(dir, "go.mod"),
		lockPath: filepath.Join(dir, "go.lock"),
	}
}

func (f *fixture) writeManifest(content string) *model.Manifest {
	f.t.Helper()
	testutil.WriteFile(f.t, f.modPath, content)
	mf, errs, _ := manifest.Parse([]byte(content))
	if len(errs) != 0 {
		f.t.Fatalf("manifest rejected: %v", errs)
	}
	return mf
}

func (f *fixture) opts() Options {
	return Options{
		ManifestPath: f.modPath,
		LockPath:     f.lockPath,
		Resolver:     resolver.Options{ModuleCache: f.cache.Root, WorkDir: f.t.TempDir()},
	}
}

// lockNow performs the lock subcommand's core flow directly against the
// packages (no subprocess needed).
func (f *fixture) lockNow(mf *model.Manifest) *model.LockFile {
	f.t.Helper()
	modules, _, err := resolver.Resolve(mf, resolver.Options{
		ModuleCache: f.cache.Root, WorkDir: f.t.TempDir()})
	if err != nil {
		f.t.Fatal(err)
	}
	// Resolution findings (missing modules, conflicts) do not block lock
	// generation; verification is what reports them.
	lf := lockfile.Generate(mf, modules)
	data, err := lockfile.Marshal(lf)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.lockPath, data, 0o644); err != nil {
		f.t.Fatal(err)
	}
	return lf
}

func basicGraph(t *testing.T, c *testutil.Cache) {
	t.Helper()
	c.Add(t, testutil.Module{Path: "example.com/a", Version: "v1.2.3",
		ModContent: "module example.com/a\n\ngo 1.23\n",
		Files:      map[string]string{"a.go": "package a\n"}})
	c.Add(t, testutil.Module{Path: "example.com/b", Version: "v0.1.0",
		ModContent: "module example.com/b\n\ngo 1.23\n\nrequire example.com/a v1.2.3\n",
		Files:      map[string]string{"b.go": "package b\n"}})
}

func TestEndToEndHappyPath(t *testing.T) {
	f := newFixture(t)
	basicGraph(t, f.cache)
	mf := f.writeManifest("module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/b v0.1.0\n")

	lf := f.lockNow(mf)
	res, errs, err := Run(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(errs) != 0 {
		for _, e := range errs {
			t.Logf("error: %v", e)
		}
		t.Fatalf("clean fixture must verify, got %d errors", len(errs))
	}
	t.Logf("REPORT for %s:", res.Report.ModulePath)
	for _, e := range res.Report.Entries {
		t.Logf("  %s status=%s locked=%s actual=%s via=%v", e.Path, e.Status, e.LockedVer, e.ActualVer, e.IntroducedBy)
	}
	if !lockfile.Equal(res.Lock, lf) {
		t.Fatal("verified lock differs from generated lock")
	}
}

func TestRepeatedLockIsIdempotent(t *testing.T) {
	f := newFixture(t)
	basicGraph(t, f.cache)
	mf := f.writeManifest("module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/b v0.1.0\n")

	first := f.lockNow(mf)
	b1, _ := os.ReadFile(f.lockPath)
	second := f.lockNow(mf)
	b2, _ := os.ReadFile(f.lockPath)
	if !lockfile.Equal(first, second) {
		t.Fatal("locking twice produced different locks")
	}
	if string(b1) != string(b2) {
		t.Fatal("locking twice changed lock bytes")
	}
	third := f.lockNow(mf)
	if !lockfile.Equal(second, third) {
		t.Fatal("third lock diverged")
	}
	t.Log("JUDGEMENT: lock applied 3x -> identical bytes and Equal")
}

func TestRepeatedVerifyIsStable(t *testing.T) {
	f := newFixture(t)
	basicGraph(t, f.cache)
	mf := f.writeManifest("module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/b v0.1.0\n")
	f.lockNow(mf)

	res1, errs1, err := Run(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	res2, errs2, err := Run(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	if len(errs1) != 0 || len(errs2) != 0 {
		t.Fatalf("verify must be clean: %v %v", errs1, errs2)
	}
	if !lockfile.Equal(res1.Lock, res2.Lock) {
		t.Fatal("repeated verify saw different locks")
	}
	if !sameReport(res1.Report, res2.Report) {
		t.Fatal("repeated verify produced different reports")
	}
	t.Log("JUDGEMENT: verify twice -> identical lock view and report")
}

func TestVerifyDistinguishesErrorClasses(t *testing.T) {
	f := newFixture(t)
	basicGraph(t, f.cache)
	mf := f.writeManifest("module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/b v0.1.0\n")
	f.lockNow(mf)

	t.Run("stale manifest change", func(t *testing.T) {
		changed := "module example.com/main\n" +
			"go 1.23\n" +
			"require example.com/b v0.2.0\n"
		testutil.WriteFile(t, f.modPath, changed)
		_, errs, err := Run(f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if !lockKind(errs, model.LockStale) {
			t.Fatalf("want lock_stale, got %v", errs)
		}
		t.Logf("JUDGEMENT: %v", errs[0])
		// restore manifest for other subtests
		testutil.WriteFile(t, f.modPath, "module example.com/main\n"+
			"go 1.23\n"+
			"require example.com/b v0.1.0\n")
	})

	t.Run("tampered lock bytes", func(t *testing.T) {
		data, _ := os.ReadFile(f.lockPath)
		// flip a digit inside a version without touching integrity line
		// (example.com/b v0.1.0 appears on a module line)
		tampered := string(data)
		tampered = replaceNth(tampered, "example.com/b v0.1.0", "example.com/b v0.1.1", 1)
		testutil.WriteFile(t, f.lockPath, tampered)
		_, errs, err := Run(f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if !lockKind(errs, model.LockTampered) {
			t.Fatalf("want lock_tampered, got %v", errs)
		}
		t.Logf("JUDGEMENT: %v", errs[0])
		f.lockNow(mf) // repair
	})

	t.Run("content hash mismatch", func(t *testing.T) {
		// Republish a's zip with different content but same version; the
		// extracted dir keeps old content, so we overwrite the zip directly.
		f.cache.Add(t, testutil.Module{Path: "example.com/a", Version: "v1.2.3",
			ModContent: "module example.com/a\n\ngo 1.23\n",
			Files:      map[string]string{"a.go": "TAMPERED CONTENT\n"}})
		_, errs, err := Run(f.opts())
		if err != nil {
			t.Fatal(err)
		}
		if !verifyKind(errs, model.VerifyHashMismatch) {
			t.Fatalf("want hash_mismatch, got %v", errs)
		}
		for _, e := range errs {
			if ve, ok := e.(*model.VerifyError); ok {
				t.Logf("JUDGEMENT module=%s kind=%s\n expected=%s\n actual=%s\n by=%v",
					ve.Module, ve.Kind, ve.Expected, ve.Actual, ve.IntroducedBy)
			}
		}
	})

	t.Run("missing dependency", func(t *testing.T) {
		f2 := newFixture(t)
		basicGraph(t, f2.cache)
		mf2 := f2.writeManifest("module example.com/main\n" +
			"go 1.23\n" +
			"require example.com/ghost v1.0.0\n")
		f2.lockNow(mf2)
		_, errs, err := Run(f2.opts())
		if err != nil {
			t.Fatal(err)
		}
		if !verifyKind(errs, model.VerifyMissingModule) {
			t.Fatalf("want missing_module, got %v", errs)
		}
		t.Logf("JUDGEMENT: %v", errs)
	})
}

func TestConcurrentVerifyIsolated(t *testing.T) {
	f := newFixture(t)
	basicGraph(t, f.cache)
	mf := f.writeManifest("module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/b v0.1.0\n")
	f.lockNow(mf)

	const n = 8
	var wg sync.WaitGroup
	errSets := make([][]error, n)
	statuses := make([][]string, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			opts := f.opts() // distinct WorkDir per goroutine
			<-start
			res, errs, err := Run(opts)
			if err != nil {
				errSets[i] = []error{err}
				return
			}
			errSets[i] = errs
			for _, e := range res.Report.Entries {
				statuses[i] = append(statuses[i], e.Path+":"+e.Status)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i := 1; i < n; i++ {
		if len(errSets[i]) != len(errSets[0]) {
			t.Fatalf("goroutine %d got %d errors, baseline has %d", i, len(errSets[i]), len(errSets[0]))
		}
		if fmt.Sprint(statuses[i]) != fmt.Sprint(statuses[0]) {
			t.Fatalf("goroutine %d report %v differs from baseline %v", i, statuses[i], statuses[0])
		}
	}
	if len(errSets[0]) != 0 {
		t.Fatalf("concurrent clean verifies must all pass, got %v", errSets[0])
	}
	t.Logf("JUDGEMENT: %d concurrent verifies, identical reports %v", n, statuses[0])
}

func TestMalformedManifestRejected(t *testing.T) {
	f := newFixture(t)
	f.writeManifestOK(t, "module example.com/main\n"+
		"go 1.23\n"+
		"require example.com/a v1.2.3\n")
	// Overwrite with a malformed manifest directly (writeManifest would fatal).
	testutil.WriteFile(t, f.modPath, "module example.com/main\ngo 1.23\nrequire example.com/a nope\n")
	_, errs, err := Run(f.opts())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range errs {
		if re, ok := e.(*model.RejectError); ok && re.Kind == model.RejectMalformedVersion {
			found = true
			t.Logf("JUDGEMENT: %v", re)
		}
	}
	if !found {
		t.Fatalf("malformed version must surface as RejectError, got %v", errs)
	}
}

func (f *fixture) writeManifestOK(t *testing.T, content string) {
	t.Helper()
	testutil.WriteFile(t, f.modPath, content)
}

func lockKind(errs []error, kind model.LockErrorKind) bool {
	for _, e := range errs {
		if le, ok := e.(*model.LockError); ok && le.Kind == kind {
			return true
		}
	}
	return false
}

func verifyKind(errs []error, kind model.VerifyErrorKind) bool {
	for _, e := range errs {
		if ve, ok := e.(*model.VerifyError); ok && ve.Kind == kind {
			return true
		}
	}
	return false
}

func sameReport(a, b *model.DiffReport) bool {
	if len(a.Entries) != len(b.Entries) {
		return false
	}
	for i := range a.Entries {
		x, y := a.Entries[i], b.Entries[i]
		if x.Path != y.Path || x.Status != y.Status ||
			x.LockedVer != y.LockedVer || x.ActualVer != y.ActualVer ||
			x.LockedHash != y.LockedHash || x.ActualHash != y.ActualHash {
			return false
		}
	}
	return true
}

func replaceNth(s, old, neu string, n int) string {
	count := 0
	idx := -1
	for {
		j := indexFrom(s, old, idx+1)
		if j < 0 {
			return s
		}
		count++
		if count == n {
			return s[:j] + neu + s[j+len(old):]
		}
		idx = j
	}
}

func indexFrom(s, sub string, from int) int {
	if from >= len(s) {
		return -1
	}
	j := func() int {
		for i := from; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return i
			}
		}
		return -1
	}()
	return j
}
