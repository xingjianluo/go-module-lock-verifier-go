package manifest

import (
	"strings"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// kindAt finds the rejection of the given kind and returns its line.
func kindAt(t *testing.T, errs []error, kind model.RejectKind) *model.RejectError {
	t.Helper()
	for _, e := range errs {
		if r, ok := e.(*model.RejectError); ok && r.Kind == kind {
			return r
		}
	}
	return nil
}

func TestParseRejectKinds(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want model.RejectKind
	}{
		{"malformed require version", "module x\ngo 1.23\nrequire example.com/a v1\n", model.RejectMalformedVersion},
		{"missing require version", "module x\ngo 1.23\nrequire example.com/a\n", model.RejectMissingVersion},
		{"missing require path", "module x\ngo 1.23\nrequire \"\" v1.2.3\n", model.RejectMissingPath},
		{"incomplete entry", "module x\ngo 1.23\nrequire (\n", model.RejectIncompleteEntry},
		{"unknown directive", "module x\ngo 1.23\nbogus thing\n", model.RejectBadDirective},
		{"duplicate replace", "module x\n" +
			"go 1.23\n" +
			"replace example.com/a v1.0.0 => example.com/b v1.0.0\n" +
			"replace example.com/a v1.0.0 => example.com/c v1.0.0\n",
			model.RejectDuplicateReplace},
		{"replace conflict too many args", "module x\n" +
			"go 1.23\n" +
			"replace example.com/a v1.0.0 v1.2.0 => example.com/b v1.0.0\n",
			model.RejectReplaceConflict},
		{"exclude without version", "module x\ngo 1.23\nexclude example.com/a\n", model.RejectExcludeNoVersion},
		{"bad go version", "module x\ngo abc\n", model.RejectMalformedVersion},
		{"missing arrow", "module x\ngo 1.23\nreplace example.com/a v1.0.0 example.com/b v1.0.0\n", model.RejectIncompleteEntry},
		{"unterminated quote", "module x\ngo 1.23\nrequire \"example.com/a v1.0.0\n", model.RejectBadDirective},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Logf("INPUT:\n%s", tc.in)
			mf, errs, err := Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("unexpected hard error: %v", err)
			}
			r := kindAt(t, errs, tc.want)
			if r == nil {
				var got []string
				for _, e := range errs {
					got = append(got, e.Error())
				}
				t.Fatalf("want rejection kind %q, got: %v (manifest: %+v)", tc.want, got, mf)
			}
			t.Logf("JUDGEMENT: kind=%s line=%d detail=%q", r.Kind, r.Line, r.Detail)
			if r.Line <= 0 {
				t.Fatalf("rejection must carry a positive line: %+v", r)
			}
		})
	}
}

func TestParseDoesNotSilentlyDropInvalidEntries(t *testing.T) {
	in := "module x\n" +
		"go 1.23\n" +
		"require (\n" +
		"	example.com/good v1.0.0\n" +
		"	example.com/bad v2\n" +
		"	example.com/alsobad\n" +
		")\n"
	t.Logf("INPUT:\n%s", in)
	mf, errs, _ := Parse([]byte(in))
	if len(mf.Requires) != 1 || mf.Requires[0].Path != "example.com/good" {
		t.Fatalf("only the good requirement should be accepted, got: %+v", mf.Requires)
	}
	if len(errs) != 2 {
		t.Fatalf("both bad entries must be reported, got %d: %v", len(errs), errs)
	}
	if kindAt(t, errs, model.RejectMalformedVersion) == nil {
		t.Fatalf("missing malformed_version rejection")
	}
	if kindAt(t, errs, model.RejectMissingVersion) == nil {
		t.Fatalf("missing missing_version rejection")
	}
	t.Logf("JUDGEMENT: accepted=1 rejected=%d distinguishable kinds confirmed", len(errs))
}

func TestParseStableOrderAndIndirect(t *testing.T) {
	in := "module example.com/main\n" +
		"go 1.23\n" +
		"require (\n" +
		"	example.com/z v1.10.0\n" +
		"	example.com/a v1.0.0 // indirect\n" +
		"	example.com/m v1.2.3\n" +
		")\n" +
		"replace example.com/z => example.com/zz v1.0.0\n" +
		"exclude example.com/m v1.2.3\n"
	first, _, _ := Parse([]byte(in))
	second, _, _ := Parse([]byte(in))
	if !equalManifests(first, second) {
		t.Fatalf("two parses of the same input differ")
	}
	// Shuffled source order must normalize to the same result.
	shuffled := "module example.com/main\n" +
		"go 1.23\n" +
		"require example.com/m v1.2.3\n" +
		"require example.com/a v1.0.0 // indirect\n" +
		"require example.com/z v1.10.0\n" +
		"exclude example.com/m v1.2.3\n" +
		"replace example.com/z => example.com/zz v1.0.0\n"
	third, _, _ := Parse([]byte(shuffled))
	if !equalManifests(first, third) {
		t.Fatalf("normalized results depend on source order")
	}
	if first.Requires[0].Path != "example.com/a" || !first.Requires[0].Indirect {
		t.Fatalf("indirect marker not preserved: %+v", first.Requires)
	}
	t.Logf("JUDGEMENT: stable order=%v indirect preserved=true", pathsOf(first.Requires))
}

func TestParseReplaceForms(t *testing.T) {
	in := "module x\ngo 1.23\n" +
		"replace example.com/a v1.0.0 => ../local-a\n" +
		"replace example.com/b => example.com/c v2.0.0\n"
	t.Logf("INPUT:\n%s", in)
	mf, errs, _ := Parse([]byte(in))
	if len(errs) != 0 {
		t.Fatalf("valid replace forms rejected: %v", errs)
	}
	var local, versioned bool
	for _, r := range mf.Replaces {
		if r.OldPath == "example.com/a" && r.Local && r.NewPath == "../local-a" {
			local = true
		}
		if r.OldPath == "example.com/b" && !r.Local && r.NewVersion == "v2.0.0" {
			versioned = true
		}
	}
	if !local || !versioned {
		t.Fatalf("replace forms not parsed: %+v", mf.Replaces)
	}
	t.Logf("JUDGEMENT: local-replace=%v version-replace=%v", local, versioned)
}

func pathsOf(rs []model.Requirement) string {
	var ps []string
	for _, r := range rs {
		ps = append(ps, r.Path)
	}
	return strings.Join(ps, ",")
}

func equalManifests(a, b *model.Manifest) bool {
	if a.ModulePath != b.ModulePath || a.GoVersion != b.GoVersion {
		return false
	}
	if len(a.Requires) != len(b.Requires) || len(a.Replaces) != len(b.Replaces) || len(a.Excludes) != len(b.Excludes) {
		return false
	}
	for i := range a.Requires {
		x, y := a.Requires[i], b.Requires[i]
		if x.Path != y.Path || x.Version != y.Version || x.Indirect != y.Indirect {
			return false
		}
	}
	for i := range a.Replaces {
		ra, rb := a.Replaces[i], b.Replaces[i]
		if ra.OldPath != rb.OldPath || ra.OldVersion != rb.OldVersion ||
			ra.NewPath != rb.NewPath || ra.NewVersion != rb.NewVersion || ra.Local != rb.Local {
			return false
		}
	}
	for i := range a.Excludes {
		x, y := a.Excludes[i], b.Excludes[i]
		if x.Path != y.Path || x.Version != y.Version {
			return false
		}
	}
	return true
}
