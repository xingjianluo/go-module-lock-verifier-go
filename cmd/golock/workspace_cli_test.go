package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// wsProject builds a two-submodule workspace with a fabricated cache.
type wsProject struct {
	dir   string
	cache *testutil.Cache
}

func newWsProject(t *testing.T) *wsProject {
	t.Helper()
	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/dep", Version: "v1.2.0",
		ModContent: "module example.com/dep\n\ngo 1.23\n",
		Files:      map[string]string{"d.go": "package dep\n"}})
	c.Add(t, testutil.Module{Path: "example.com/extra", Version: "v0.3.0",
		ModContent: "module example.com/extra\n\ngo 1.23\n",
		Files:      map[string]string{"e.go": "package extra\n"}})

	dir := t.TempDir()
	testutil.WriteFile(t, filepath.Join(dir, "golock.work"),
		"name example.com/ws\ngo 1.23\nuse (\n\t./mod-a\n\t./mod-b\n)\n")
	testutil.WriteFile(t, filepath.Join(dir, "mod-a", "go.mod"),
		"module example.com/mod-a\ngo 1.23\nrequire example.com/dep v1.2.0\n")
	testutil.WriteFile(t, filepath.Join(dir, "mod-b", "go.mod"),
		"module example.com/mod-b\ngo 1.23\nrequire example.com/dep v1.2.0\nrequire example.com/extra v0.3.0\n")
	return &wsProject{dir: dir, cache: c}
}

// run executes the CLI and returns combined output (for logging) plus
// stdout alone (for machine-readable assertions).
func (p *wsProject) run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	combined, _, err := p.runSplit(t, args...)
	return combined, err
}

func (p *wsProject) runSplit(t *testing.T, args ...string) (combined, stdout string, err error) {
	t.Helper()
	var outBuf, errBuf strings.Builder
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = p.dir
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	err = cmd.Run()
	return outBuf.String() + errBuf.String(), outBuf.String(), err
}

// TestCLIWorkspaceLockAudit exercises the multi-module flow end to end:
// lock the whole workspace, audit it, and confirm attribution to
// submodules appears in the lock.
func TestCLIWorkspaceLockAudit(t *testing.T) {
	bin := buildCLI(t)
	p := newWsProject(t)

	out, err := p.run(t, bin, "lock", "-workspace", "golock.work", "-cache", p.cache.Root)
	t.Logf("WORKSPACE LOCK OUTPUT:\n%s", out)
	if err != nil {
		t.Fatalf("workspace lock failed: %v\n%s", err, out)
	}
	lockData, rerr := os.ReadFile(filepath.Join(p.dir, "go.lock"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	text := string(lockData)
	for _, want := range []string{"example.com/dep", "example.com/extra", "example.com/mod-a", "example.com/mod-b"} {
		if !strings.Contains(text, want) {
			t.Fatalf("lock must mention %q:\n%s", want, text)
		}
	}
	t.Logf("LOCK CONTENT:\n%s", text)

	out, err = p.run(t, bin, "audit", "-workspace", "golock.work", "-cache", p.cache.Root)
	t.Logf("WORKSPACE AUDIT OUTPUT:\n%s", out)
	if err != nil {
		t.Fatalf("clean audit must pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "VERIFY OK") {
		t.Fatalf("audit must report success:\n%s", out)
	}

	// Audit must be read-only: lock bytes unchanged.
	after, _ := os.ReadFile(filepath.Join(p.dir, "go.lock"))
	if string(after) != text {
		t.Fatal("audit modified the lock file")
	}
	t.Log("JUDGEMENT audit is read-only, lock bytes identical")
}

// TestCLIWorkspaceConflictJSON makes two submodules disagree on a version
// and checks the JSON output classifies the conflict with both submodules
// named, so CI can branch on category/kind without parsing text.
func TestCLIWorkspaceConflictJSON(t *testing.T) {
	bin := buildCLI(t)
	p := newWsProject(t)
	testutil.WriteFile(t, filepath.Join(p.dir, "mod-b", "go.mod"),
		"module example.com/mod-b\ngo 1.23\nrequire example.com/dep v1.3.0\n")

	out, stdout, err := p.runSplit(t, bin, "lock", "-workspace", "golock.work", "-cache", p.cache.Root, "-json")
	t.Logf("CONFLICT OUTPUT:\n%s", out)
	if err == nil {
		t.Fatal("conflicting workspace must fail")
	}
	var res struct {
		OK     bool `json:"ok"`
		Issues []struct {
			Category string   `json:"category"`
			Kind     string   `json:"kind"`
			Module   string   `json:"module"`
			Expected string   `json:"expected"`
			Actual   string   `json:"actual"`
			Sources  []string `json:"sources"`
		} `json:"issues"`
	}
	if jerr := json.Unmarshal([]byte(stdout), &res); jerr != nil {
		t.Fatalf("stdout must be machine-readable JSON: %v\n%s", jerr, stdout)
	}
	if res.OK {
		t.Fatal("ok must be false on conflict")
	}
	if len(res.Issues) != 1 {
		t.Fatalf("want exactly one structured issue, got %+v", res.Issues)
	}
	iss := res.Issues[0]
	t.Logf("ISSUE category=%s kind=%s module=%s expected=%s actual=%s sources=%v",
		iss.Category, iss.Kind, iss.Module, iss.Expected, iss.Actual, iss.Sources)
	if iss.Category != "workspace" || iss.Kind != "requirement_conflict" {
		t.Fatalf("CI must see workspace/requirement_conflict, got %s/%s", iss.Category, iss.Kind)
	}
	joined := strings.Join(iss.Sources, "\n")
	if !strings.Contains(joined, "example.com/mod-a") || !strings.Contains(joined, "example.com/mod-b") {
		t.Fatalf("issue must name both submodules: %v", iss.Sources)
	}

	// A failed lock run must not leave any lock behind.
	if _, serr := os.Stat(filepath.Join(p.dir, "go.lock")); !os.IsNotExist(serr) {
		t.Fatal("failed lock run must not leave a lock file")
	}
	leftovers, _ := filepath.Glob(filepath.Join(p.dir, ".golock-*.tmp"))
	if len(leftovers) > 0 {
		t.Fatalf("failed run left temp files: %v", leftovers)
	}
	t.Log("JUDGEMENT conflict classified in JSON; no lock or temp file left")
}

// TestCLIFailedResolutionLeavesNoLock points the workspace at a module that
// is not in the cache; the lock run must fail atomically.
func TestCLIFailedResolutionLeavesNoLock(t *testing.T) {
	bin := buildCLI(t)
	p := newWsProject(t)
	testutil.WriteFile(t, filepath.Join(p.dir, "mod-a", "go.mod"),
		"module example.com/mod-a\ngo 1.23\nrequire example.com/ghost v9.9.9\n")

	out, err := p.run(t, bin, "lock", "-workspace", "golock.work", "-cache", p.cache.Root, "-json")
	t.Logf("FAILED LOCK OUTPUT:\n%s", out)
	if err == nil {
		t.Fatal("unresolvable workspace must fail")
	}
	if !strings.Contains(out, "missing_module") {
		t.Fatalf("failure must be classified missing_module:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(p.dir, "go.lock")); !os.IsNotExist(serr) {
		t.Fatal("failed resolution must not leave a lock file")
	}
	leftovers, _ := filepath.Glob(filepath.Join(p.dir, ".golock-*.tmp"))
	if len(leftovers) > 0 {
		t.Fatalf("failed run left temp files: %v", leftovers)
	}
	t.Log("JUDGEMENT missing_module classified; lock absent; no temp files")
}

// TestCLIAuditJSONStable runs the JSON audit twice and requires identical
// bytes, then tampers the lock and checks the tamper classification.
func TestCLIAuditJSONStable(t *testing.T) {
	bin := buildCLI(t)
	p := newWsProject(t)
	if out, err := p.run(t, bin, "lock", "-workspace", "golock.work", "-cache", p.cache.Root); err != nil {
		t.Fatalf("lock: %v\n%s", err, out)
	}

	runAudit := func() string {
		t.Helper()
		out, stdout, err := p.runSplit(t, bin, "audit", "-workspace", "golock.work", "-cache", p.cache.Root, "-json")
		if err != nil {
			t.Fatalf("clean audit: %v\n%s", err, out)
		}
		return stdout
	}
	first, second := runAudit(), runAudit()
	t.Logf("AUDIT JSON:\n%s", first)
	if first != second {
		t.Fatal("repeated JSON audits must be byte-identical")
	}
	var res struct {
		OK      bool   `json:"ok"`
		Subject string `json:"subject"`
		Modules []struct {
			Path         string   `json:"path"`
			Status       string   `json:"status"`
			IntroducedBy []string `json:"introduced_by"`
		} `json:"modules"`
	}
	if err := json.Unmarshal([]byte(first), &res); err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Subject != "example.com/ws" {
		t.Fatalf("unexpected result: ok=%v subject=%q", res.OK, res.Subject)
	}
	for _, m := range res.Modules {
		t.Logf("MODULE %s status=%s via=%v", m.Path, m.Status, m.IntroducedBy)
		if m.Status != "ok" {
			t.Fatalf("clean audit module %s status=%s", m.Path, m.Status)
		}
	}

	// Tamper the lock: audit must classify lock_tampered in JSON.
	lockPath := filepath.Join(p.dir, "go.lock")
	data, _ := os.ReadFile(lockPath)
	tampered := strings.Replace(string(data), "v1.2.0", "v1.2.1", 1)
	if err := os.WriteFile(lockPath, []byte(tampered), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := p.run(t, bin, "audit", "-workspace", "golock.work", "-cache", p.cache.Root, "-json")
	t.Logf("TAMPERED AUDIT JSON:\n%s", out)
	if err == nil {
		t.Fatal("tampered lock must fail audit")
	}
	if !strings.Contains(out, `"lock_tampered"`) {
		t.Fatalf("JSON must classify lock_tampered:\n%s", out)
	}
	t.Log("JUDGEMENT repeated audits identical; tamper classified in JSON")
}
