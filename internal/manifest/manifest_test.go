package manifest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func parseT(t *testing.T, src string) (*Manifest, error) {
	t.Helper()
	p := &Parser{}
	m, err := p.Parse(context.Background(), []byte(src))
	t.Logf("--- input ---\n%s", src)
	if err != nil {
		t.Logf("--- rejected ---\n%s", err)
	}
	return m, err
}

func TestValidNormalizationStable(t *testing.T) {
	src := `module example.com/main

go 1.25

require b.com/m v1.2.0
require (
	a.com/m v1.0.0 // indirect
	c.com/m v0.1.0
)

exclude b.com/m v1.2.0
replace a.com/m => ./local
replace b.com/m v1.2.0 => d.com/m v2.0.0+incompatible
`
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "local"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Parser{ModuleDir: dir}
	m, err := p.Parse(context.Background(), []byte(src))
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if got := m.Requires[0].Path; got != "a.com/m" {
		t.Fatalf("requires not sorted by path, got %q first", got)
	}
	if m.Requires[0].Kind != KindIndirect {
		t.Fatal("indirect marker lost")
	}
	h1 := m.CanonicalHash()
	// Reorder directives: normalized hash must be identical.
	src2 := `module example.com/main
go 1.25
replace b.com/m v1.2.0 => d.com/m v2.0.0+incompatible
replace a.com/m => ./local
exclude b.com/m v1.2.0
require c.com/m v0.1.0
require a.com/m v1.0.0 // indirect
require b.com/m v1.2.0
`
	m2, err := p.Parse(context.Background(), []byte(src2))
	if err != nil {
		t.Fatalf("unexpected errors: %v", err)
	}
	if h2 := m2.CanonicalHash(); h1 != h2 {
		t.Fatalf("normalization unstable:\n%s\n%s", h1, h2)
	}
	t.Logf("canonical hash stable: %s", h1)
}

func TestRejectedEntryCodes(t *testing.T) {
	src := "module example.com/main\n" +
		"require good.com/m v1.0.0\n" +
		"require badver.com/m v1.0\n" +          // bad version
		"require nover.com/m\n" +                // missing version
		"require \"\" v1.0.0\n" +                // invalid token -> bad path
		"exclude e.com/m v1.x.0\n" +             // bad version in exclude
		"replace r.com/m =>\n" +                 // empty RHS
		"replace r.com/m => other.com/m\n" +     // module replacement missing version
		"replace r.com/m => other.com/m v1\n" +  // bad replacement version
		"require a.com/m v1.0.0\n" +
		"require a.com/m v1.0.0\n" // duplicate require: Go allows same version, keep it
	_, err := parseT(t, src)
	if err == nil {
		t.Fatal("expected rejection aggregate, got nil")
	}
	errs, ok := AsEntryErrors(err)
	if !ok {
		t.Fatalf("want manifest.Errors, got %T", err)
	}
	want := map[EntryErrorCode]bool{
		EntryErrBadVersion:      false,
		EntryErrEmptyVersion:    false,
		EntryErrBadPath:         false,
		EntryErrInvalidReplace:  false,
	}
	for _, e := range errs {
		var ee *EntryError
		if !errors.As(e, &ee) {
			t.Fatalf("non-entry error: %v", e)
		}
		if _, tracked := want[ee.Code]; tracked {
			want[ee.Code] = true
		}
		t.Logf("line %d kind=%s code=%v", ee.Line, ee.Kind, ee.Code)
	}
	for code, found := range want {
		if !found {
			t.Fatalf("missing expected rejection code %v; all: %v", code, want)
		}
	}
}

func TestDuplicateAndLocalDirRules(t *testing.T) {
	src := "module example.com/main\n" +
		"replace a.com/m v1.0.0 => x.com/m v1.1.0\n" +
		"replace a.com/m v1.0.0 => y.com/m v1.2.0\n" +
		"exclude e.com/m v1.0.0\n" +
		"exclude e.com/m v1.0.0\n"
	_, err := parseT(t, src)
	if err == nil {
		t.Fatal("expected duplicate errors")
	}
	var dupR, dupE bool
	for _, e := range err.(Errors) {
		var ee *EntryError
		errors.As(e, &ee)
		if ee.Code == EntryErrDuplicateReplace {
			dupR = true
		}
		if ee.Code == EntryErrDuplicateExclude {
			dupE = true
		}
	}
	if !dupR || !dupE {
		t.Fatalf("duplicate codes missing: replace=%v exclude=%v", dupR, dupE)
	}

	// Missing local directory must be rejected when parsing from disk.
	dir := t.TempDir()
	gomod := filepath.Join(dir, "go.mod")
	if err := os.WriteFile(gomod, []byte("module example.com/main\nreplace a.com/m => ./nope\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Parser{}
	if _, err := p.ParseFile(context.Background(), gomod); err == nil {
		t.Fatal("expected EntryErrLocalDirMissing")
	} else {
		var found bool
		for _, e := range err.(Errors) {
			var ee *EntryError
			errors.As(e, &ee)
			if ee.Code == EntryErrLocalDirMissing {
				found = true
			}
		}
		if !found {
			t.Fatalf("want local-dir-missing, got %v", err)
		}
		t.Logf("missing local dir correctly rejected:\n%s", err)
	}
}

func TestUnterminatedBlock(t *testing.T) {
	src := "module x/y\nrequire (\n\ta.com/m v1.0.0\n"
	_, err := parseT(t, src)
	if err == nil {
		t.Fatal("expected malformed directive error")
	}
	var found bool
	for _, e := range err.(Errors) {
		var ee *EntryError
		errors.As(e, &ee)
		if ee.Code == EntryErrMalformedDirective {
			found = true
		}
	}
	if !found {
		t.Fatalf("want malformed-directive, got %v", err)
	}
}
