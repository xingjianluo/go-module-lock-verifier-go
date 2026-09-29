package graph

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/cache"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
)

func writeMod(t *testing.T, root, path, version, gomod string) {
	t.Helper()
	encPath := func(p string) string {
		b := make([]byte, 0, len(p))
		for i := 0; i < len(p); i++ {
			c := p[i]
			if c >= 'A' && c <= 'Z' {
				b = append(b, '!', c+32)
			} else {
				b = append(b, c)
			}
		}
		return string(b)
	}
	dir := filepath.Join(root, filepath.FromSlash(encPath(path))+"@"+version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib.go"), []byte("package lib\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dl := filepath.Join(root, "cache", "download",
		filepath.FromSlash(encPath(path)), "@v")
	if err := os.MkdirAll(dl, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dl, version+".mod"), []byte(gomod), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustParse(t *testing.T, src, dir string) *manifest.Manifest {
	t.Helper()
	p := &manifest.Parser{ModuleDir: dir}
	m, err := p.Parse(context.Background(), []byte(src))
	if err != nil {
		t.Fatalf("manifest rejected: %v", err)
	}
	return m
}

func find(mods []*SelectedModule, path string) *SelectedModule {
	for _, mm := range mods {
		if mm.Path == path {
			return mm
		}
	}
	return nil
}

func TestResolveTransitiveAndMVS(t *testing.T) {
	root := t.TempDir()
	writeMod(t, root, "example.com/a", "v1.0.0",
		"module example.com/a\nrequire example.com/b v1.1.0 // indirect\n")
	writeMod(t, root, "example.com/b", "v1.1.0",
		"module example.com/b\nrequire example.com/c v0.5.0 // indirect\n")
	writeMod(t, root, "example.com/b", "v1.2.0",
		"module example.com/b\nrequire example.com/c v0.6.0 // indirect\n")
	writeMod(t, root, "example.com/c", "v0.5.0", "module example.com/c\n")
	writeMod(t, root, "example.com/c", "v0.6.0", "module example.com/c\n")

	c, _ := cache.New(root)
	src := "module example.com/main\n" +
		"require example.com/a v1.0.0\n" +
		"require example.com/b v1.2.0 // indirect\n"
	m := mustParse(t, src, root)
	r := &Resolver{Cache: c}
	res, err := r.Resolve(context.Background(), m)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got := find(res.Modules, "example.com/c"); got == nil {
		t.Fatal("transitive c missing")
	} else if got.Version != "v0.6.0" {
		t.Fatalf("MVS picked %s, want v0.6.0 (max of a->v0.5.0, b->v0.6.0)", got.Version)
	} else {
		t.Logf("c selected %s via chain %v", got.Version, got.IntroducedBy)
	}
	if got := find(res.Modules, "example.com/a"); !got.Direct {
		t.Fatal("a should be direct")
	}
	if got := find(res.Modules, "example.com/c"); got.Direct {
		t.Fatal("c should be indirect")
	}

	// Repeatability.
	res2, err := r.Resolve(context.Background(), m)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Modules) != len(res2.Modules) {
		t.Fatal("repeated resolution differs in length")
	}
	for i := range res.Modules {
		if res.Modules[i].Path != res2.Modules[i].Path ||
			res.Modules[i].Version != res2.Modules[i].Version {
			t.Fatalf("repeated resolution differs at %d", i)
		}
	}
	t.Logf("resolved %d modules, repeat identical", len(res.Modules))
}

func TestResolveMissingDependency(t *testing.T) {
	root := t.TempDir()
	writeMod(t, root, "example.com/a", "v1.0.0",
		"module example.com/a\nrequire ghost.com/x v1.0.0 // indirect\n")
	c, _ := cache.New(root)
	m := mustParse(t, "module main/x\nrequire example.com/a v1.0.0\n", root)
	_, err := (&Resolver{Cache: c}).Resolve(context.Background(), m)
	if err == nil {
		t.Fatal("expected missing dependency")
	}
	var gerrs Errors
	if !errors.As(err, &gerrs) {
		t.Fatalf("want graph.Errors, got %T", err)
	}
	var found bool
	for _, e := range gerrs {
		if e.Code == GraphErrMissingDependency && e.Module.Path == "ghost.com/x" {
			found = true
		}
		t.Logf("classified: code=%v module=%s", e.Code, e.Module)
	}
	if !found {
		t.Fatalf("missing-dependency not classified: %v", err)
	}
	if !errors.Is(err, &Error{Code: GraphErrMissingDependency}) {
		t.Fatal("errors.Is failed on aggregate")
	}
}

func TestResolveExcludeConflict(t *testing.T) {
	root := t.TempDir()
	writeMod(t, root, "example.com/a", "v1.0.0",
		"module example.com/a\nrequire example.com/b v1.2.0 // indirect\n")
	writeMod(t, root, "example.com/b", "v1.2.0", "module example.com/b\n")
	writeMod(t, root, "example.com/b", "v1.0.0", "module example.com/b\n")
	c, _ := cache.New(root)
	src := "module main/x\n" +
		"require example.com/a v1.0.0\n" +
		"exclude example.com/b v1.2.0\n"
	m := mustParse(t, src, root)
	_, err := (&Resolver{Cache: c}).Resolve(context.Background(), m)
	if err == nil {
		t.Fatal("expected version conflict for excluded selected version")
	}
	var gerrs Errors
	errors.As(err, &gerrs)
	var ce *Error
	for _, e := range gerrs {
		if e.Code == GraphErrVersionConflict {
			ce = e
		}
	}
	if ce == nil {
		t.Fatalf("want version-conflict, got %v", err)
	}
	if ce.Module.Path != "example.com/b" || ce.Module.Version != "v1.2.0" {
		t.Fatalf("conflict targets %s, want b@v1.2.0", ce.Module)
	}
	if len(ce.Required) == 0 {
		t.Fatal("conflict must report requirement evidence")
	}
	t.Logf("conflict: %s | requirements=%v", ce, ce.Required)
}

func TestResolveLocalReplacementExpands(t *testing.T) {
	root := t.TempDir()
	writeMod(t, root, "example.com/dep", "v2.0.0", "module example.com/dep\n")
	local := filepath.Join(root, "localfork")
	if err := os.MkdirAll(local, 0o755); err != nil {
		t.Fatal(err)
	}
	localMod := "module example.com/fork\nrequire example.com/dep v2.0.0 // indirect\n"
	if err := os.WriteFile(filepath.Join(local, "go.mod"), []byte(localMod), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(local, "f.go"), []byte("package fork\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, _ := cache.New(root)
	src := "module main/x\n" +
		"require example.com/a v1.0.0\n" +
		"replace example.com/a => ./localfork\n"
	m := mustParse(t, src, root)
	res, err := (&Resolver{Cache: c}).Resolve(context.Background(), m)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	a := find(res.Modules, "example.com/a")
	if a == nil || !a.Replaced || !a.Local || a.LocalDir != local {
		t.Fatalf("a not marked local replacement: %+v", a)
	}
	if d := find(res.Modules, "example.com/dep"); d == nil || d.Version != "v2.0.0" {
		t.Fatalf("transitive dep from local fork missing/wrong: %+v", d)
	} else {
		t.Logf("local replacement expanded; dep chain=%v", d.IntroducedBy)
	}
}
