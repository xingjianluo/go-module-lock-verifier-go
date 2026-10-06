#!/usr/bin/env bash
# Offline multi-module workspace demo: fabricates a GOMODCACHE and a
# two-submodule workspace, then exercises workspace lock, JSON audit,
# conflict reporting and stale detection. Requires: go (no network access).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
CACHE="$WORK/cache"
PROJ="$WORK/ws"
mkdir -p "$PROJ/mod-a" "$PROJ/mod-b"
trap 'rm -rf "$WORK"' EXIT

echo "== workdir: $WORK (fake cache + workspace) =="

mkdir -p "$WORK/seed"
cat > "$WORK/seed/go.mod" <<'GOMOD'
module seed

go 1.23
GOMOD
cat > "$WORK/seed/main.go" <<'GO'
package main

import (
	"archive/zip"
	"os"
	"path/filepath"
)

func add(cache, path, ver, mod string, files map[string]string) {
	dl := filepath.Join(cache, "cache", "download", path, "@v")
	must(os.MkdirAll(dl, 0o755))
	must(os.WriteFile(filepath.Join(dl, ver+".mod"), []byte(mod), 0o644))
	dir := filepath.Join(cache, path+"@"+ver)
	must(os.MkdirAll(dir, 0o755))
	must(os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644))
	for n, c := range files {
		must(os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644))
	}
	f, err := os.Create(filepath.Join(dl, ver+".zip"))
	must(err)
	defer f.Close()
	zw := zip.NewWriter(f)
	put := func(n, c string) {
		w, err := zw.Create(path + "@" + ver + "/" + n)
		must(err)
		_, _ = w.Write([]byte(c))
	}
	put("go.mod", mod)
	for n, c := range files {
		put(n, c)
	}
	must(zw.Close())
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func main() {
	cache := os.Args[1]
	add(cache, "example.com/dep", "v1.2.0",
		"module example.com/dep\n\ngo 1.23\n",
		map[string]string{"d.go": "package dep\n"})
	add(cache, "example.com/extra", "v0.3.0",
		"module example.com/extra\n\ngo 1.23\n",
		map[string]string{"e.go": "package extra\n"})
}
GO

(cd "$WORK/seed" && GOTOOLCHAIN=local go run . "$CACHE")

cat > "$PROJ/golock.work" <<'WS'
name example.com/ws
go 1.23
use (
	./mod-a
	./mod-b
)
WS
cat > "$PROJ/mod-a/go.mod" <<'MOD'
module example.com/mod-a

go 1.23

require example.com/dep v1.2.0
MOD
cat > "$PROJ/mod-b/go.mod" <<'MOD'
module example.com/mod-b

go 1.23

require (
	example.com/dep v1.2.0
	example.com/extra v0.3.0
)
MOD

GOTOOLCHAIN=local go build -o "$WORK/golock" "$ROOT/cmd/golock"
golock() { "$WORK/golock" "$@" -cache "$CACHE"; }
run() { (cd "$PROJ" && "$@"); }

echo; echo "== 1. workspace lock =="
run golock lock -workspace golock.work

echo; echo "== 2. workspace audit, machine-readable (expect ok:true) =="
run golock audit -workspace golock.work -json

echo; echo "== 3. submodule disagreement (expect workspace/requirement_conflict) =="
cp "$PROJ/go.lock" "$WORK/lock.before"
cat > "$PROJ/mod-b/go.mod" <<'MOD'
module example.com/mod-b

go 1.23

require example.com/dep v1.3.0
MOD
run golock lock -workspace golock.work -json || true
echo "-- failed run must leave the previous lock untouched:"
if cmp -s "$WORK/lock.before" "$PROJ/go.lock" && ! ls "$PROJ"/.golock-*.tmp >/dev/null 2>&1; then
	echo "   OK: lock unchanged, no temp files"
else
	echo "   FAIL: lock was modified or temp files remain"; exit 1
fi
# restore agreement and re-lock
cat > "$PROJ/mod-b/go.mod" <<'MOD'
module example.com/mod-b

go 1.23

require (
	example.com/dep v1.2.0
	example.com/extra v0.3.0
)
MOD
run golock lock -workspace golock.work

echo; echo "== 4. change a submodule declaration (expect lock_stale) =="
cat > "$PROJ/mod-b/go.mod" <<'MOD'
module example.com/mod-b

go 1.23

require example.com/dep v1.2.0
MOD
run golock audit -workspace golock.work -json || true

echo; echo "== demo complete =="
