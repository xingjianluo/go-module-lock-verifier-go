package verify

import (
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/testutil"
)

func testutilModNew() testutil.Module {
	return testutil.Module{Path: "example.com/new", Version: "v0.9.0",
		ModContent: "module example.com/new\n\ngo 1.23\n",
		Files:      map[string]string{"n.go": "package n\n"}}
}

func testutilWrite(f *fixture, path, content string) {
	testutil.WriteFile(f.t, path, content)
}
