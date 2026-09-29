package lockfile

import (
	"strings"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

func sampleManifest() *model.Manifest {
	mf, errs, _ := manifest.Parse([]byte(
		"module example.com/main\n" +
			"go 1.23\n" +
			"require example.com/a v1.2.3\n" +
			"require example.com/b v0.1.0 // indirect\n"))
	if len(errs) != 0 {
		panic("bad sample manifest")
	}
	return mf
}

func sampleModules() []model.LockedModule {
	return []model.LockedModule{
		{Path: "example.com/b", Version: "v0.1.0", Indirect: true,
			Hash: "sha256:bbb", IntroducedBy: []string{"example.com/a", "example.com/main"}},
		{Path: "example.com/a", Version: "v1.2.3", Indirect: false,
			Hash: "sha256:aaa", IntroducedBy: []string{"example.com/main"}},
	}
}

func TestGenerateMarshalIsStableAndSorted(t *testing.T) {
	mf := sampleManifest()
	l1 := Generate(mf, sampleModules())
	b1, err := Marshal(l1)
	if err != nil {
		t.Fatal(err)
	}
	l2 := Generate(mf, sampleModules())
	b2, _ := Marshal(l2)
	if string(b1) != string(b2) {
		t.Fatalf("repeated generation is not deterministic\n%s----\n%s", b1, b2)
	}
	t.Logf("LOCK CONTENT:\n%s", b1)
	// modules must be sorted regardless of input order
	if !strings.Contains(string(b1), "module example.com/a ") ||
		strings.Index(string(b1), "example.com/a") > strings.Index(string(b1), "example.com/b") {
		t.Fatalf("modules not sorted:\n%s", b1)
	}
	// via lists must be sorted/unique
	if strings.Contains(string(b1), "via example.com/a example.com/main") == false {
		t.Fatalf("via list not normalized:\n%s", b1)
	}

	parsed, err := Unmarshal(b1)
	if err != nil {
		t.Fatalf("round trip failed: %v", err)
	}
	if !Equal(l1, parsed) {
		t.Fatalf("round trip changed the lock")
	}
	t.Log("JUDGEMENT: generate twice -> identical bytes; unmarshal -> Equal")
}

func TestTamperDetectedAfterAnyEdit(t *testing.T) {
	mf := sampleManifest()
	lf := Generate(mf, sampleModules())
	data, _ := Marshal(lf)

	tampered := strings.Replace(string(data), "module example.com/a v1.2.3",
		"module example.com/a v1.2.4", 1)
	t.Logf("TAMPERED INPUT:\n%s", tampered)
	_, err := Unmarshal([]byte(tampered))
	le, ok := err.(*model.LockError)
	if !ok {
		t.Fatalf("want *LockError, got %T: %v", err, err)
	}
	if le.Kind != model.LockTampered {
		t.Fatalf("want lock_tampered, got %s", le.Kind)
	}
	t.Logf("JUDGEMENT: kind=%s expected=%s actual=%s", le.Kind, le.Expected, le.Actual)
}

func TestStaleVsTamperedAreDistinct(t *testing.T) {
	mf := sampleManifest()
	lf := Generate(mf, sampleModules())
	data, _ := Marshal(lf)

	// 1) Untampered lock + changed manifest -> stale, not tampered.
	changed, errs, _ := manifest.Parse([]byte(
		"module example.com/main\n" +
			"go 1.23\n" +
			"require example.com/a v1.3.0\n" +
			"require example.com/b v0.1.0 // indirect\n"))
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	parsed, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("intact lock failed to parse: %v", err)
	}
	err = CheckManifest(parsed, changed)
	le, ok := err.(*model.LockError)
	if !ok || le.Kind != model.LockStale {
		t.Fatalf("want lock_stale, got %v", err)
	}
	t.Logf("STALE CASE: kind=%s expected-hash=%s actual-hash=%s", le.Kind, le.Expected, le.Actual)

	// 2) Tampered lock + same manifest -> tampered at parse time.
	tampered := strings.Replace(string(data), "sha256:aaa", "sha256:dead", 1)
	if _, err := Unmarshal([]byte(tampered)); err == nil {
		t.Fatal("expected tampering to be rejected")
	} else if le2, ok := err.(*model.LockError); !ok || le2.Kind != model.LockTampered {
		t.Fatalf("want lock_tampered, got %v", err)
	} else {
		t.Logf("TAMPER CASE: kind=%s", le2.Kind)
	}
}

func TestMalformedLocks(t *testing.T) {
	cases := map[string]model.LockErrorKind{
		"not a lock at all":                                   model.LockMalformed,
		"# golock v1\nintegrity sha256:x\n":                   model.LockTampered,
		"# golock v1\nintegrity sha256:x\nformat-version 9\n": model.LockTampered,
	}
	for in, kind := range cases {
		_, err := Unmarshal([]byte(in))
		if err == nil {
			t.Fatalf("input must be rejected: %q", in)
		}
		le, ok := err.(*model.LockError)
		if !ok {
			t.Fatalf("want LockError, got %T", err)
		}
		if le.Kind != kind {
			t.Fatalf("input %q: want %s, got %s", in, kind, le.Kind)
		}
		t.Logf("JUDGEMENT %q -> %s (%s)", in, le.Kind, le.Detail)
	}
}

func TestUnsupportedFormatVersion(t *testing.T) {
	// Build a payload with format-version 2 and a matching integrity so only
	// the version check trips.
	payload := "format-version 2\nmodule-name x\nmanifest-hash sha256:00\n"
	sum := integrityOf(payload)
	data := []byte("# golock v1\nintegrity " + sum + "\n" + payload)
	_, err := Unmarshal(data)
	le, ok := err.(*model.LockError)
	if !ok || le.Kind != model.LockVersionUnsupported {
		t.Fatalf("want lock_version_unsupported, got %v", err)
	}
	t.Logf("JUDGEMENT: kind=%s actual=%s", le.Kind, le.Actual)
}
