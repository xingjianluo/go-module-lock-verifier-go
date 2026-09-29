package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

// TestCLILockVerify builds the CLI binary and exercises the offline lock and
// verify flow against a fabricated cache.
func TestCLILockVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("binary build in short mode")
	}
	bin := filepath.Join(t.TempDir(), "golock")
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}

	c := testutil.NewCache(t)
	c.Add(t, testutil.Module{Path: "example.com/a", Version: "v1.2.3",
		ModContent: "module example.com/a\n\ngo 1.23\n",
		Files:      map[string]string{"a.go": "package a\n"}})

	proj := t.TempDir()
	manifest := "module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/a v1.2.3\n"
	testutil.WriteFile(t, filepath.Join(proj, "go.mod"), manifest)

	lock := exec.Command(bin, "lock", "-cache", c.Root)
	lock.Dir = proj
	out, err := lock.CombinedOutput()
	t.Logf("LOCK OUTPUT:\n%s", out)
	if err != nil {
		t.Fatalf("lock failed: %v\n%s", err, out)
	}
	data, rerr := os.ReadFile(filepath.Join(proj, "go.lock"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	if !strings.Contains(string(data), "manifest-hash") {
		t.Fatalf("lock missing manifest-hash:\n%s", data)
	}

	// Second lock must report up-to-date and not rewrite bytes.
	info1, _ := os.Stat(filepath.Join(proj, "go.lock"))
	second := exec.Command(bin, "lock", "-cache", c.Root)
	second.Dir = proj
	out2, err := second.CombinedOutput()
	if err != nil {
		t.Fatalf("second lock failed: %v\n%s", err, out2)
	}
	t.Logf("SECOND LOCK OUTPUT:\n%s", out2)
	if !strings.Contains(string(out2), "up to date") {
		t.Fatalf("expected idempotent lock, got:\n%s", out2)
	}
	info2, _ := os.Stat(filepath.Join(proj, "go.lock"))
	if info1.Size() != info2.Size() {
		t.Fatal("idempotent lock changed file size")
	}

	verify := exec.Command(bin, "verify", "-cache", c.Root)
	verify.Dir = proj
	vout, err := verify.CombinedOutput()
	t.Logf("VERIFY OUTPUT:\n%s", vout)
	if err != nil {
		t.Fatalf("verify failed: %v\n%s", err, vout)
	}

	// Tamper with the lock -> verify must fail and classify.
	tampered := strings.Replace(string(data), "v1.2.3", "v1.2.4", 1)
	os.WriteFile(filepath.Join(proj, "go.lock"), []byte(tampered), 0o644)
	badCmd := exec.Command(bin, "verify", "-cache", c.Root)
	badCmd.Dir = proj
	badOut, err := badCmd.CombinedOutput()
	if err == nil {
		t.Fatal("tampered verify unexpectedly succeeded")
	}
	t.Logf("TAMPERED VERIFY OUTPUT:\n%s", badOut)
	if !strings.Contains(string(badOut), "lock_tampered") {
		t.Fatalf("tampered lock must be classified lock_tampered:\n%s", badOut)
	}
}

// TestCLIMalformedManifest ensures a bad manifest is rejected with a
// non-zero exit and the specific reason is printed.
func TestCLIMalformedManifest(t *testing.T) {
	if testing.Short() {
		t.Skip("binary build in short mode")
	}
	bin := filepath.Join(t.TempDir(), "golock")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build cli: %v\n%s", err, out)
	}
	proj := t.TempDir()
	testutil.WriteFile(t, filepath.Join(proj, "go.mod"),
		"module example.com/main\ngo 1.23\nrequire example.com/a bogus-version\n")
	cmd := exec.Command(bin, "lock", "-cache", t.TempDir())
	cmd.Dir = proj
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("malformed manifest must fail")
	}
	t.Logf("MALFORMED OUTPUT:\n%s", out)
	if !strings.Contains(string(out), "malformed_version") {
		t.Fatalf("reason must be distinguishable, got: %s", out)
	}
}
