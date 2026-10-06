package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

func buildCLI(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("binary build in short mode")
	}
	bin := filepath.Join(t.TempDir(), "golock")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}
	return bin
}

// workspaceProject lays out a two-module workspace with a shared dependency.
func workspaceProject(t *testing.T) (proj string, cache *testutil.Cache) {
	t.Helper()
	cache = testutil.NewCache(t)
	cache.Add(t, testutil.Module{Path: "example.com/dep", Version: "v1.2.0",
		ModContent: "module example.com/dep\n\ngo 1.22\n",
		Files:      map[string]string{"dep.go": "package dep\n"}})
	cache.Add(t, testutil.Module{Path: "example.com/dep", Version: "v1.3.0",
		ModContent: "module example.com/dep\n\ngo 1.22\n",
		Files:      map[string]string{"dep.go": "package dep // 1.3\n"}})

	proj = t.TempDir()
	testutil.WriteFile(t, filepath.Join(proj, "golock.work"),
		"name example.com/ws\nuse ./svc-a\nuse ./svc-b\n")
	testutil.WriteFile(t, filepath.Join(proj, "svc-a", "go.mod"),
		"module example.com/a\ngo 1.22\nrequire example.com/dep v1.2.0\n")
	testutil.WriteFile(t, filepath.Join(proj, "svc-b", "go.mod"),
		"module example.com/b\ngo 1.22\nrequire example.com/dep v1.3.0\n")
	return proj, cache
}

func runCLI(t *testing.T, bin, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	t.Logf("$ golock %s\n%s", strings.Join(args, " "), out)
	return string(out), err
}

type machineReport struct {
	Command string `json:"command"`
	OK      bool   `json:"ok"`
	Subject string `json:"subject"`
	Modules []string `json:"modules"`
	Findings []struct {
		Category string `json:"category"`
		Kind     string `json:"kind"`
		Module   string `json:"module"`
	} `json:"findings"`
}

func TestCLIWorkspaceLockAndAudit(t *testing.T) {
	bin := buildCLI(t)
	proj, cache := workspaceProject(t)

	out, err := runCLI(t, bin, proj, "lock", "-work", "golock.work", "-cache", cache.Root)
	if err != nil {
		t.Fatalf("workspace lock failed: %v\n%s", err, out)
	}
	lockData, rerr := os.ReadFile(filepath.Join(proj, "go.lock"))
	if rerr != nil {
		t.Fatalf("lock not written: %v", rerr)
	}
	// MVS across submodules: the highest requested version wins.
	if !strings.Contains(string(lockData), "example.com/dep v1.3.0") {
		t.Fatalf("merged lock must select v1.3.0:\n%s", lockData)
	}
	if !strings.Contains(string(lockData), "module-name example.com/ws") {
		t.Fatalf("lock must carry workspace identity:\n%s", lockData)
	}

	out, err = runCLI(t, bin, proj, "audit", "-work", "golock.work", "-cache", cache.Root, "-json")
	if err != nil {
		t.Fatalf("workspace audit failed: %v\n%s", err, out)
	}
	var rep machineReport
	if jerr := json.Unmarshal([]byte(jsonLine(t, out)), &rep); jerr != nil {
		t.Fatalf("audit output not JSON: %v\n%s", jerr, out)
	}
	t.Logf("audit report: ok=%v subject=%s modules=%v", rep.OK, rep.Subject, rep.Modules)
	if !rep.OK || rep.Command != "audit" || rep.Subject != "example.com/ws" {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if len(rep.Modules) != 2 || len(rep.Findings) != 0 {
		t.Fatalf("unexpected report content: %+v", rep)
	}

	// Re-lock must be a byte-identical no-op.
	out, err = runCLI(t, bin, proj, "lock", "-work", "golock.work", "-cache", cache.Root)
	if err != nil || !strings.Contains(out, "up to date") {
		t.Fatalf("re-lock not idempotent: %v\n%s", err, out)
	}
}

// jsonLine extracts the JSON document from combined output (human lines may
// precede it on stdout for lock; audit -json prints only JSON).
func jsonLine(t *testing.T, out string) string {
	t.Helper()
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in output:\n%s", out)
	}
	return out[start:]
}

func TestCLIWorkspaceConflictFailsWithoutLock(t *testing.T) {
	bin := buildCLI(t)
	proj, cache := workspaceProject(t)
	// svc-b replaces the shared dep locally; svc-a replaces it elsewhere.
	testutil.WriteFile(t, filepath.Join(proj, "svc-a", "go.mod"),
		"module example.com/a\ngo 1.22\nrequire example.com/dep v1.2.0\nreplace example.com/dep => example.com/fork v1.0.1\n")
	testutil.WriteFile(t, filepath.Join(proj, "svc-b", "go.mod"),
		"module example.com/b\ngo 1.22\nrequire example.com/dep v1.3.0\nreplace example.com/dep => ../local\n")

	out, err := runCLI(t, bin, proj, "lock", "-work", "golock.work", "-cache", cache.Root, "-json")
	if err == nil {
		t.Fatalf("conflicting workspace must fail:\n%s", out)
	}
	var rep machineReport
	if jerr := json.Unmarshal([]byte(jsonLine(t, out)), &rep); jerr != nil {
		t.Fatalf("failure output not JSON: %v\n%s", jerr, out)
	}
	if rep.OK || len(rep.Findings) != 1 {
		t.Fatalf("report: %+v", rep)
	}
	f := rep.Findings[0]
	t.Logf("conflict finding: category=%s kind=%s module=%s", f.Category, f.Kind, f.Module)
	if f.Category != "workspace" || f.Kind != "workspace_replace_conflict" {
		t.Fatalf("finding: %+v", f)
	}
	if _, serr := os.Stat(filepath.Join(proj, "go.lock")); !os.IsNotExist(serr) {
		t.Fatal("failed lock run left a lock file behind")
	}
}

func TestCLILockFailureLeavesNoPartialLock(t *testing.T) {
	bin := buildCLI(t)
	proj, cache := workspaceProject(t)
	// Point at an empty cache: the dep cannot resolve.
	emptyCache := testutil.NewCache(t)
	_ = cache

	out, err := runCLI(t, bin, proj, "lock", "-work", "golock.work", "-cache", emptyCache.Root)
	if err == nil {
		t.Fatalf("lock with missing modules must fail:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(proj, "go.lock")); !os.IsNotExist(serr) {
		t.Fatal("failed run left a partial lock behind")
	}
	// And no temp files either.
	leftover, _ := filepath.Glob(filepath.Join(proj, ".golock-*"))
	if len(leftover) != 0 {
		t.Fatalf("temp files left behind: %v", leftover)
	}
}

func TestCLIAuditJSONClassifiesTamper(t *testing.T) {
	bin := buildCLI(t)
	proj, cache := workspaceProject(t)
	if out, err := runCLI(t, bin, proj, "lock", "-work", "golock.work", "-cache", cache.Root); err != nil {
		t.Fatalf("lock: %v\n%s", err, out)
	}
	// Tamper with the lock content.
	lockPath := filepath.Join(proj, "go.lock")
	data, _ := os.ReadFile(lockPath)
	tampered := strings.Replace(string(data), "v1.3.0", "v1.3.1", 1)
	if err := os.WriteFile(lockPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runCLI(t, bin, proj, "audit", "-work", "golock.work", "-cache", cache.Root, "-json")
	if err == nil {
		t.Fatalf("tampered audit must fail:\n%s", out)
	}
	var rep machineReport
	if jerr := json.Unmarshal([]byte(jsonLine(t, out)), &rep); jerr != nil {
		t.Fatalf("not JSON: %v\n%s", jerr, out)
	}
	if rep.OK || len(rep.Findings) == 0 {
		t.Fatalf("report: %+v", rep)
	}
	f := rep.Findings[0]
	t.Logf("tamper finding: category=%s kind=%s", f.Category, f.Kind)
	if f.Category != "lock" || f.Kind != "lock_tampered" {
		t.Fatalf("finding: %+v", f)
	}
	// Audit is read-only: the tampered lock stays exactly as we left it.
	after, _ := os.ReadFile(lockPath)
	if string(after) != tampered {
		t.Fatal("audit modified the lock file")
	}
}

func TestCLIConcurrentLocksAndAudits(t *testing.T) {
	bin := buildCLI(t)
	proj, cache := workspaceProject(t)
	if out, err := runCLI(t, bin, proj, "lock", "-work", "golock.work", "-cache", cache.Root); err != nil {
		t.Fatalf("initial lock: %v\n%s", err, out)
	}
	want, _ := os.ReadFile(filepath.Join(proj, "go.lock"))

	// Hammer the same cache and the same lock path from concurrent
	// processes: conclusions must not flap and the lock must stay whole.
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, 2*n)
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			out, err := runCLI(t, bin, proj, "lock", "-work", "golock.work", "-cache", cache.Root)
			if err == nil {
				data, _ := os.ReadFile(filepath.Join(proj, "go.lock"))
				if string(data) != string(want) {
					t.Errorf("concurrent lock changed lock bytes:\n%s", data)
				}
			} else {
				t.Errorf("concurrent lock failed: %v\n%s", err, out)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			out, err := runCLI(t, bin, proj, "audit", "-work", "golock.work", "-cache", cache.Root, "-json")
			if err != nil {
				t.Errorf("concurrent audit failed: %v\n%s", err, out)
			}
		}(i)
	}
	wg.Wait()
	_ = errs
	// No temp files leaked into the project dir.
	leftover, _ := filepath.Glob(filepath.Join(proj, ".golock-*"))
	if len(leftover) != 0 {
		t.Fatalf("temp files left behind: %v", leftover)
	}
}
