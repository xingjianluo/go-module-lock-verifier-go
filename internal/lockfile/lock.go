// Package lockfile serializes, parses and applies lock files and generates
// locks from a resolved module model.
//
// Format (version "1", line oriented, UTF-8):
//
//	# golock v1
//	manifest-hash <sha256:...>
//	integrity <sha256:...>
//	module <path> <version> [indirect] [hash sha256:...]
//	       [replace-by <path> <version>] [replace-local <path>]
//	       [via <p1> <p2> ...]
//
// "integrity" hashes every line except the integrity line itself; editing any
// other line after generation is therefore detectable as tampering, while
// manifest-hash mismatches specifically mean declarations changed and the
// lock is stale.
package lockfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
)

const (
	// FormatVersion is the lock format version emitted and accepted.
	FormatVersion = "1"
	header        = "# golock v1"
)

// Generate builds a lock for the manifest and its resolved modules. The module
// list is sorted deterministically and IntroducedBy lists are normalized, so
// the same inputs always produce an identical lock.
func Generate(m *model.Manifest, modules []model.LockedModule) *model.LockFile {
	out := make([]model.LockedModule, len(modules))
	copy(out, modules)
	for i := range out {
		out[i].IntroducedBy = sortedUnique(out[i].IntroducedBy)
		if out[i].Replacement != nil {
			cp := *out[i].Replacement
			out[i].Replacement = &cp
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return &model.LockFile{
		FormatVersion: FormatVersion,
		ModulePath:    m.ModulePath,
		ManifestHash:  manifest.CanonicalHash(m),
		Modules:       out,
	}
}

// Marshal renders the lock file, embedding a fresh integrity hash. Applying
// Marshal to the same LockFile twice yields identical bytes.
func Marshal(lf *model.LockFile) ([]byte, error) {
	if lf == nil {
		return nil, &model.LockError{Kind: model.LockMalformed, Detail: "nil lock"}
	}
	payload := marshalPayload(lf)
	sum := sha256.Sum256([]byte(payload))
	var b strings.Builder
	b.WriteString(header)
	b.WriteString("\n")
	b.WriteString("integrity sha256:")
	b.WriteString(hex.EncodeToString(sum[:]))
	b.WriteString("\n")
	b.WriteString(payload)
	return []byte(b.String()), nil
}

func marshalPayload(lf *model.LockFile) string {
	var b strings.Builder
	fmt.Fprintf(&b, "format-version %s\n", lf.FormatVersion)
	fmt.Fprintf(&b, "module-name %s\n", lf.ModulePath)
	fmt.Fprintf(&b, "manifest-hash %s\n", lf.ManifestHash)
	for _, mod := range lf.Modules {
		line := []string{"module", mod.Path, mod.Version}
		if mod.Indirect {
			line = append(line, "indirect")
		}
		if mod.Hash != "" {
			line = append(line, "hash", mod.Hash)
		}
		if r := mod.Replacement; r != nil {
			if r.Local {
				line = append(line, "replace-local", r.NewPath)
			} else {
				line = append(line, "replace-by", r.NewPath, r.NewVersion)
			}
		}
		if len(mod.IntroducedBy) > 0 {
			line = append(line, "via")
			line = append(line, mod.IntroducedBy...)
		}
		b.WriteString(strings.Join(line, " "))
		b.WriteString("\n")
	}
	return b.String()
}

// Unmarshal parses lock text. Structural problems come back as
// *model.LockError with kind LockMalformed / LockVersionUnsupported; a
// structurally valid file whose recorded integrity does not match its content
// returns LockTampered.
func Unmarshal(data []byte) (*model.LockFile, error) {
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	lf := &model.LockFile{}
	var integrity string
	var payloadLines []string

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != header {
		return nil, &model.LockError{Kind: model.LockMalformed, Detail: "missing or unknown header"}
	}
	for i := 1; i < len(lines); i++ {
		raw := lines[i]
		trim := strings.TrimSpace(raw)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		toks, ok := splitFields(trim)
		if !ok {
			return nil, malformed(i+1, "unterminated quoted field")
		}
		switch toks[0] {
		case "integrity":
			if len(toks) != 2 {
				return nil, malformed(i+1, "integrity expects one hash")
			}
			integrity = toks[1]
		default:
			payloadLines = append(payloadLines, trim)
		}
	}

	if integrity == "" {
		return nil, &model.LockError{Kind: model.LockMalformed, Detail: "missing integrity line"}
	}
	sum := sha256.Sum256([]byte(strings.Join(payloadLines, "\n") + "\n"))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if integrity != want {
		return nil, &model.LockError{
			Kind: model.LockTampered, Expected: want, Actual: integrity,
			Detail: "lock content does not match its integrity hash",
		}
	}

	seenModule := map[string]bool{}
	for i, line := range payloadLines {
		toks, _ := splitFields(line)
		switch toks[0] {
		case "format-version":
			if len(toks) != 2 {
				return nil, malformed(i, "format-version expects one value")
			}
			lf.FormatVersion = toks[1]
		case "module-name":
			if len(toks) != 2 {
				return nil, malformed(i, "module-name expects one value")
			}
			lf.ModulePath = toks[1]
		case "manifest-hash":
			if len(toks) != 2 {
				return nil, malformed(i, "manifest-hash expects one hash")
			}
			lf.ManifestHash = toks[1]
		case "module":
			mod, err := parseModuleLine(toks, i)
			if err != nil {
				return nil, err
			}
			if seenModule[mod.Path] {
				return nil, malformed(i, "duplicate module entry: "+mod.Path)
			}
			seenModule[mod.Path] = true
			lf.Modules = append(lf.Modules, mod)
		default:
			return nil, malformed(i, "unknown lock keyword: "+toks[0])
		}
	}
	if lf.FormatVersion != FormatVersion {
		return nil, &model.LockError{
			Kind: model.LockVersionUnsupported, Actual: lf.FormatVersion,
			Detail: "only format version " + FormatVersion + " is supported",
		}
	}
	if lf.ManifestHash == "" {
		return nil, &model.LockError{Kind: model.LockMalformed, Detail: "missing manifest-hash"}
	}
	sort.SliceStable(lf.Modules, func(i, j int) bool {
		return lf.Modules[i].Path < lf.Modules[j].Path
	})
	return lf, nil
}

func parseModuleLine(toks []string, line int) (model.LockedModule, error) {
	if len(toks) < 3 {
		return model.LockedModule{}, malformed(line, "module requires path and version")
	}
	mod := model.LockedModule{Path: toks[1], Version: toks[2]}
	rest := toks[3:]
	for i := 0; i < len(rest); i++ {
		switch rest[i] {
		case "indirect":
			mod.Indirect = true
		case "hash":
			if i+1 >= len(rest) {
				return mod, malformed(line, "hash without value")
			}
			mod.Hash, i = rest[i+1], i+1
		case "replace-by":
			if i+2 >= len(rest) {
				return mod, malformed(line, "replace-by needs path and version")
			}
			mod.Replacement = &model.ReplaceRule{
				OldPath: mod.Path, NewPath: rest[i+1], NewVersion: rest[i+2],
			}
			i += 2
		case "replace-local":
			if i+1 >= len(rest) {
				return mod, malformed(line, "replace-local without path")
			}
			mod.Replacement = &model.ReplaceRule{
				OldPath: mod.Path, NewPath: rest[i+1], Local: true,
			}
			i++
		case "via":
			mod.IntroducedBy = append(mod.IntroducedBy, rest[i+1:]...)
			i = len(rest)
		default:
			return mod, malformed(line, "unexpected module field: "+rest[i])
		}
	}
	return mod, nil
}

// IntegrityHash computes the integrity hash a lock's payload would carry.
// Comparing it with the value stored on disk distinguishes a structurally
// valid lock whose content was edited after generation (tampering).
func IntegrityHash(lf *model.LockFile) string {
	sum := sha256.Sum256([]byte(marshalPayload(lf)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CheckManifest returns a LockStale error when the lock's manifest hash does
// not match the current normalized manifest. It never mutates the lock.
func CheckManifest(lf *model.LockFile, m *model.Manifest) error {
	got := manifest.CanonicalHash(m)
	if lf.ManifestHash != got {
		return &model.LockError{
			Kind: model.LockStale, Expected: got, Actual: lf.ManifestHash,
			Detail: "manifest changed since the lock was generated; re-run lock",
		}
	}
	if lf.ModulePath != m.ModulePath {
		return &model.LockError{
			Kind: model.LockStale,
			Detail: fmt.Sprintf("module path changed: lock has %q, manifest has %q",
				lf.ModulePath, m.ModulePath),
		}
	}
	return nil
}

// Equal reports whether two locks describe identical effective selections
// (same manifest hash and same module/version/hash/replacement sets).
// Applying or generating the same lock twice always yields Equal locks.
func Equal(a, b *model.LockFile) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.ManifestHash != b.ManifestHash || a.ModulePath != b.ModulePath {
		return false
	}
	if len(a.Modules) != len(b.Modules) {
		return false
	}
	am := map[string]model.LockedModule{}
	for _, mod := range a.Modules {
		am[mod.Path] = mod
	}
	for _, bm := range b.Modules {
		amod, ok := am[bm.Path]
		if !ok {
			return false
		}
		if amod.Version != bm.Version || amod.Hash != bm.Hash ||
			amod.Indirect != bm.Indirect {
			return false
		}
		if (amod.Replacement == nil) != (bm.Replacement == nil) {
			return false
		}
		if amod.Replacement != nil &&
			(amod.Replacement.NewPath != bm.Replacement.NewPath ||
				amod.Replacement.NewVersion != bm.Replacement.NewVersion ||
				amod.Replacement.Local != bm.Replacement.Local) {
			return false
		}
		if !equalStrings(sortedUnique(amod.IntroducedBy), sortedUnique(bm.IntroducedBy)) {
			return false
		}
	}
	return true
}

func sortedUnique(in []string) []string {
	set := map[string]struct{}{}
	for _, s := range in {
		set[s] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func malformed(line int, detail string) *model.LockError {
	return &model.LockError{Kind: model.LockMalformed, Detail: "line " + strconv.Itoa(line) + ": " + detail}
}

// splitFields splits on whitespace, honoring double quotes for local paths
// containing spaces. ok is false on an unterminated quote.
func splitFields(s string) ([]string, bool) {
	var fields []string
	for i := 0; i < len(s); {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t') {
			i++
		}
		if i >= len(s) {
			break
		}
		if s[i] == '"' {
			start := i
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' {
					i++
				}
				i++
			}
			if i >= len(s) {
				return nil, false
			}
			v, err := strconv.Unquote(s[start : i+1])
			if err != nil {
				return nil, false
			}
			fields = append(fields, v)
			i++
		} else {
			start := i
			for i < len(s) && s[i] != ' ' && s[i] != '\t' {
				i++
			}
			fields = append(fields, s[start:i])
		}
	}
	return fields, true
}
