package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

// CanonicalText renders a manifest as deterministic, ordering-independent
// text. Two manifests that express identical declarations (direct, indirect,
// replaces and excludes) always produce identical bytes; source ordering of
// directives is irrelevant.
func CanonicalText(m *model.Manifest) string {
	var b strings.Builder
	b.WriteString("module ")
	b.WriteString(m.ModulePath)
	b.WriteString("\ngo ")
	b.WriteString(m.GoVersion)
	b.WriteString("\n")

	reqs := append([]model.Requirement(nil), m.Requires...)
	sort.SliceStable(reqs, func(i, j int) bool {
		if reqs[i].Path != reqs[j].Path {
			return reqs[i].Path < reqs[j].Path
		}
		return reqs[i].Version < reqs[j].Version
	})
	for _, r := range reqs {
		b.WriteString("require ")
		b.WriteString(r.Path)
		b.WriteString(" ")
		b.WriteString(r.Version)
		if r.Indirect {
			b.WriteString(" // indirect")
		}
		b.WriteString("\n")
	}

	repls := append([]model.ReplaceRule(nil), m.Replaces...)
	sort.SliceStable(repls, func(i, j int) bool {
		if repls[i].OldPath != repls[j].OldPath {
			return repls[i].OldPath < repls[j].OldPath
		}
		return repls[i].OldVersion < repls[j].OldVersion
	})
	for _, r := range repls {
		b.WriteString("replace ")
		b.WriteString(r.OldPath)
		if r.OldVersion != "" {
			b.WriteString(" ")
			b.WriteString(r.OldVersion)
		}
		b.WriteString(" => ")
		b.WriteString(r.NewPath)
		if !r.Local && r.NewVersion != "" {
			b.WriteString(" ")
			b.WriteString(r.NewVersion)
		}
		b.WriteString("\n")
	}

	exs := append([]model.ExcludeRule(nil), m.Excludes...)
	sort.SliceStable(exs, func(i, j int) bool {
		if exs[i].Path != exs[j].Path {
			return exs[i].Path < exs[j].Path
		}
		return exs[i].Version < exs[j].Version
	})
	for _, e := range exs {
		b.WriteString("exclude ")
		b.WriteString(e.Path)
		b.WriteString(" ")
		b.WriteString(e.Version)
		b.WriteString("\n")
	}
	return b.String()
}

// CanonicalHash returns "sha256:<hex>" over the canonical text.
func CanonicalHash(m *model.Manifest) string {
	sum := sha256.Sum256([]byte(CanonicalText(m)))
	return "sha256:" + hex.EncodeToString(sum[:])
}
