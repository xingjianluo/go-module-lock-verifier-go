// Command golock generates and verifies reproducible module locks offline.
//
// Usage:
//
//	golock lock   [-mod go.mod] [-lock go.lock] [-cache GOMODCACHE]
//	golock verify [-mod go.mod] [-lock go.lock] [-cache GOMODCACHE]
//
// Both subcommands are read-only against the module cache. lock writes the
// lock file atomically only when regeneration changes it; verify never
// writes it.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/resolver"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/verify"
)

type flags struct {
	modPath  string
	lockPath string
	cache    string
	workDir  string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	cache := os.Getenv("GOMODCACHE")
	if cache == "" {
		if home, err := os.UserHomeDir(); err == nil {
			cache = filepath.Join(home, "go", "pkg", "mod")
		}
	}
	f := flags{modPath: "go.mod", lockPath: "go.lock", cache: cache}
	if err := parseFlags(&f, args); err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		os.Exit(2)
	}
	if f.workDir == "" {
		d, err := os.MkdirTemp("", "golock-*")
		if err != nil {
			fmt.Fprintln(os.Stderr, "golock: workdir:", err)
			os.Exit(2)
		}
		f.workDir = d
		defer os.RemoveAll(d)
	}

	var code int
	switch cmd {
	case "lock":
		code = runLock(f)
	case "verify":
		code = runVerify(f)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

func runLock(f flags) int {
	mf, rejects, err := manifest.ParseFile(f.modPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock: read manifest:", err)
		return 2
	}
	if len(rejects) > 0 {
		fmt.Fprintln(os.Stderr, "golock: manifest rejected:")
		for _, r := range rejects {
			fmt.Fprintln(os.Stderr, "  -", r)
		}
		return 1
	}

	modules, resErrs, err := resolver.Resolve(mf, resolver.Options{
		ModuleCache: f.cache, WorkDir: f.workDir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		return 2
	}
	// Resolution errors (missing modules, conflicts) do not prevent producing
	// a lock of what was selectable, but they are surfaced and fail the run.
	generated := lockfile.Generate(mf, modules)
	data, err := lockfile.Marshal(generated)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock: marshal lock:", err)
		return 2
	}

	var existing *model.LockFile
	if old, rerr := os.ReadFile(f.lockPath); rerr == nil {
		lf, uerr := lockfile.Unmarshal(old)
		if uerr == nil {
			existing = lf
		}
	}
	if existing != nil && lockfile.Equal(existing, generated) {
		fmt.Printf("lock %s: already up to date (%d modules)\n", f.lockPath, len(modules))
	} else {
		if werr := os.WriteFile(f.lockPath, data, 0o644); werr != nil {
			fmt.Fprintln(os.Stderr, "golock: write lock:", werr)
			return 2
		}
		fmt.Printf("lock %s: wrote %d modules\n", f.lockPath, len(modules))
	}
	for _, e := range resErrs {
		fmt.Fprintln(os.Stderr, "  !", e)
	}
	if len(resErrs) > 0 {
		return 1
	}
	return 0
}

func runVerify(f flags) int {
	res, errs, err := verify.Run(verify.Options{
		ManifestPath: f.modPath,
		LockPath:     f.lockPath,
		Resolver: resolver.Options{
			ModuleCache: f.cache, WorkDir: f.workDir,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		return 2
	}
	if res != nil && res.Report != nil {
		printReport(res.Report)
	}
	if len(errs) == 0 {
		fmt.Println("VERIFY OK: lock matches manifest and local cache")
		return 0
	}
	fmt.Fprintln(os.Stderr, "VERIFY FAILED:")
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "  -", classify(e), e)
	}
	return 1
}

func classify(e error) string {
	switch e.(type) {
	case *model.RejectError:
		return "[manifest]"
	case *model.LockError:
		return "[lock]"
	case *model.VerifyError:
		return "[cache]"
	default:
		return "[error]"
	}
}

func printReport(r *model.DiffReport) {
	fmt.Printf("module: %s\n", r.ModulePath)
	fmt.Printf("%-30s %-14s %-12s %s\n", "PATH", "STATUS", "LOCKED", "ACTUAL")
	for _, e := range r.Entries {
		ver := e.ActualVer
		if e.Status == "extra-locked" {
			ver = "-"
		}
		fmt.Printf("%-30s %-14s %-12s %s\n", e.Path, e.Status, e.LockedVer, ver)
		if len(e.IntroducedBy) > 0 {
			fmt.Printf("%-30s   via: %v\n", "", e.IntroducedBy)
		}
		if e.Status == "hash-mismatch" || e.Status == "missing" {
			fmt.Printf("%-30s   expected hash: %s\n", "", e.LockedHash)
			fmt.Printf("%-30s   actual hash:   %s\n", "", e.ActualHash)
		}
	}
}

func parseFlags(f *flags, args []string) error {
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", a)
			}
			i++
			return args[i], nil
		}
		switch a {
		case "-mod", "-f":
			v, err := next()
			if err != nil {
				return err
			}
			f.modPath = v
		case "-lock", "-o":
			v, err := next()
			if err != nil {
				return err
			}
			f.lockPath = v
		case "-cache":
			v, err := next()
			if err != nil {
				return err
			}
			f.cache = v
		case "-workdir":
			v, err := next()
			if err != nil {
				return err
			}
			f.workDir = v
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `golock - offline reproducible module locks

  golock lock   [-mod go.mod] [-lock go.lock] [-cache DIR]
  golock verify [-mod go.mod] [-lock go.lock] [-cache DIR]

Flags:
  -mod PATH     manifest file (default go.mod)
  -lock PATH    lock file (default go.lock)
  -cache DIR    module cache (default $GOMODCACHE)
  -workdir DIR  private scratch directory (default fresh temp dir)
`)
}
