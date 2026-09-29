package resolver

import (
	"testing"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

func TestContentHashStableAndContentSensitive(t *testing.T) {
	c1 := testutil.NewCache(t)
	c1.Add(t, testutil.Module{Path: "example.com/h", Version: "v1.0.0",
		ModContent: "module example.com/h\n\ngo 1.23\n",
		Files:      map[string]string{"x.go": "package h\n", "sub/y.go": "package sub\n"}})
	cache1 := NewCache(c1.Root)
	h1, ok := cache1.ContentHash("example.com/h", "v1.0.0")
	if !ok {
		t.Fatal("hash from zip must be available")
	}
	h1Again, ok := cache1.ContentHash("example.com/h", "v1.0.0")
	if !ok || h1 != h1Again {
		t.Fatalf("hash must be deterministic: %s vs %s", h1, h1Again)
	}

	// An independent cache with identical content must hash identically.
	c2 := testutil.NewCache(t)
	c2.Add(t, testutil.Module{Path: "example.com/h", Version: "v1.0.0",
		ModContent: "module example.com/h\n\ngo 1.23\n",
		Files:      map[string]string{"sub/y.go": "package sub\n", "x.go": "package h\n"}})
	h2, _ := NewCache(c2.Root).ContentHash("example.com/h", "v1.0.0")
	if h1 != h2 {
		t.Fatalf("hash must not depend on insertion order: %s vs %s", h1, h2)
	}
	t.Logf("JUDGEMENT: identical content -> identical hash %s (independent caches, different map order)", h1)

	// Changed content must change the hash.
	c3 := testutil.NewCache(t)
	c3.Add(t, testutil.Module{Path: "example.com/h", Version: "v1.0.0",
		ModContent: "module example.com/h\n\ngo 1.23\n",
		Files:      map[string]string{"x.go": "CHANGED\n", "sub/y.go": "package sub\n"}})
	h3, _ := NewCache(c3.Root).ContentHash("example.com/h", "v1.0.0")
	if h3 == h1 {
		t.Fatal("changed file content must change the hash")
	}
	t.Logf("JUDGEMENT: changed content -> different hash %s != %s", h3, h1)

	// Directory-only fallback (remove the zip) must also hash and be stable.
	c4 := testutil.NewCache(t)
	c4.Add(t, testutil.Module{Path: "example.com/d", Version: "v1.0.0",
		ModContent: "module example.com/d\n\ngo 1.23\n",
		Files:      nil}) // no zip
	cache4 := NewCache(c4.Root)
	hd, ok := cache4.ContentHash("example.com/d", "v1.0.0")
	if !ok {
		t.Fatal("directory fallback hash must be available")
	}
	hd2, _ := cache4.ContentHash("example.com/d", "v1.0.0")
	if hd != hd2 {
		t.Fatal("directory hash must be stable across calls")
	}
	t.Logf("JUDGEMENT: directory-only fallback stable hash %s", hd)
}

func TestEscapedPath(t *testing.T) {
	cases := map[string]string{
		"github.com/foo/bar":  "github.com/foo/bar",
		"example.com/Foo/bar": "example.com/!foo/bar",
		"Acme.io/v2":          "!acme.io/v2",
	}
	for in, want := range cases {
		if got := escapedPath(in); got != want {
			t.Errorf("escapedPath(%q) = %q, want %q", in, got, want)
		}
	}
}
