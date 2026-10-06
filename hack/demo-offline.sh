#!/usr/bin/env bash
# Offline end-to-end demo: builds a fake GOMODCACHE + project in temp dirs and
# exercises lock / verify plus tamper, content drift, and stale declarations.
# Requires: go (the tool itself never accesses the network).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d)"
CACHE="$WORK/cache"
PROJ="$WORK/proj"
mkdir -p "$PROJ"
trap 'rm -rf "$WORK"' EXIT

echo "== workdir: $WORK (fake cache + project) =="

# Build a tiny helper binary that fabricates a GOMODCACHE layout.
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

func add(cache, path, ver, mod string, files map[string]string, withDir bool) {
	dl := filepath.Join(cache, "cache", "download", path, "@v")
	must(os.MkdirAll(dl, 0o755))
	must(os.WriteFile(filepath.Join(dl, ver+".mod"), []byte(mod), 0o644))
	dir := filepath.Join(cache, path+"@"+ver)
	if withDir {
		must(os.MkdirAll(dir, 0o755))
		must(os.WriteFile(filepath.Join(dir, "go.mod"), []byte(mod), 0o644))
		for n, c := range files {
			fp := filepath.Join(dir, n)
			must(os.MkdirAll(filepath.Dir(fp), 0o755))
			must(os.WriteFile(fp, []byte(c), 0o644))
		}
	}
	f, err := os.Create(filepath.Join(dl, ver+".zip"))
	must(err)
	defer f.Close()
	zw := zip.NewWriter(f)
	put := func(n, c string) {
		w, err := zw.Create(path + "@" + ver + "/" + n)
		must(err)
		_, must2 := w.Write([]byte(c))
		_ = must2
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
	mode := os.Args[2]
	content := "package a\n"
	withDir := true
	switch mode {
	case "tamper":
		content = "PACKAGE A WAS TAMPERED\n"
		withDir = false // force hashing the zip alone
	}
	add(cache, "example.com/a", "v1.2.3",
		"module example.com/a\n\ngo 1.23\n",
		map[string]string{"a.go": content}, withDir)
}
GO

(cd "$WORK/seed" && GOTOOLCHAIN=local go run . "$CACHE" ok)

cat > "$PROJ/go.mod" <<'MOD'
module example.com/demo

go 1.23

require example.com/a v1.2.3
MOD

GOTOOLCHAIN=local go build -o "$WORK/golock" "$ROOT/cmd/golock"
golock() { "$WORK/golock" "$@" -cache "$CACHE"; }
run() { (cd "$PROJ" && "$@"); }

echo; echo "== 1. lock =="
run golock lock

echo; echo "== 2. verify (expect OK) =="
run golock verify

echo; echo "== 3. tamper lock file (expect lock_tampered) =="
sed -i 's/v1.2.3 hash/v1.2.4 hash/' "$PROJ/go.lock"
run golock verify || true
run golock lock >/dev/null  # regenerate to repair

echo; echo "== 4. tamper cached content (expect hash_mismatch) =="
(cd "$WORK/seed" && GOTOOLCHAIN=local go run . "$CACHE" tamper)
run golock verify || true
(cd "$WORK/seed" && GOTOOLCHAIN=local go run . "$CACHE" ok)  # restore
run golock lock >/dev/null

echo; echo "== 5. change manifest (expect lock_stale) =="
cat > "$PROJ/go.mod" <<'MOD'
module example.com/demo

go 1.23

require example.com/a v1.2.4
MOD
run golock verify || true

echo; echo "== 6. workspace: lock two submodules as one =="
WS="$PROJ/ws"
mkdir -p "$WS/svc-a" "$WS/svc-b"
cat > "$WS/golock.work" <<'WORK'
name example.com/demo-ws
use ./svc-a
use ./svc-b
WORK
cat > "$WS/svc-a/go.mod" <<'MOD'
module example.com/svc-a

go 1.23

require example.com/a v1.2.3
MOD
cat > "$WS/svc-b/go.mod" <<'MOD'
module example.com/svc-b

go 1.23

require example.com/a v1.2.3
MOD
runws() { (cd "$WS" && "$@"); }
runws golock lock -work golock.work
echo "-- merged go.lock --"
cat "$WS/go.lock"

echo; echo "== 7. workspace audit -json (expect ok:true) =="
runws golock audit -work golock.work -json

echo; echo "== 8. workspace replace conflict (expect workspace_replace_conflict, no lock) =="
cat > "$WS/svc-a/go.mod" <<'MOD'
module example.com/svc-a

go 1.23

require example.com/a v1.2.3

replace example.com/a => example.com/fork-a v1.2.4
MOD
cat > "$WS/svc-b/go.mod" <<'MOD'
module example.com/svc-b

go 1.23

require example.com/a v1.2.3

replace example.com/a => ../local-a
MOD
rm -f "$WS/go.lock"
runws golock lock -work golock.work || true
if [ -e "$WS/go.lock" ]; then
  echo "BUG: conflicting workspace left a lock behind"
else
  echo "OK: no lock written on conflict"
fi

echo; echo "== demo complete =="
