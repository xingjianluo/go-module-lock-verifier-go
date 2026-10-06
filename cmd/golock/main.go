// Command golock generates and verifies reproducible module locks offline,
// for a single module or a multi-module workspace.
//
// Usage:
//
//	golock lock   [-mod go.mod | -workspace golock.work] [-lock go.lock] [-cache GOMODCACHE] [-json]
//	golock verify [-mod go.mod | -workspace golock.work] [-lock go.lock] [-cache GOMODCACHE] [-json]
//	golock audit  [-mod go.mod | -workspace golock.work] [-lock go.lock] [-cache GOMODCACHE] [-json]
//
// All subcommands are read-only against the module cache. lock writes the
// lock file atomically and only when regeneration changes it and resolution
// succeeded completely; verify/audit never write it. audit is an alias of
// verify emphasizing the read-only change-audit use case.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/report"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/resolver"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/verify"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/workspace"
)

type flags struct {
	modPath   string
	lockPath  string
	cache     string
	workDir   string
	workspace string
	jsonOut   bool
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
	case "verify", "audit":
		code = runAudit(f, cmd)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

// loadDeclarations resolves the declaration set for a run: either a single
// go.mod or a whole workspace merged into one effective manifest.
func loadDeclarations(f flags) (*model.Manifest, []error, error) {
	if f.workspace != "" {
		ws, werrs, err := workspace.Load(f.workspace)
		if err != nil {
			return nil, nil, err
		}
		if len(werrs) > 0 {
			return nil, werrs, nil
		}
		mf, merrs := ws.Merge()
		if len(merrs) > 0 {
			return nil, merrs, nil
		}
		return mf, nil, nil
	}
	mf, rejects, err := manifest.ParseFile(f.modPath)
	if err != nil {
		return nil, nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(rejects) > 0 {
		return nil, rejects, nil
	}
	return mf, nil, nil
}

func (f flags) resolverOptions() resolver.Options {
	return resolver.Options{ModuleCache: f.cache, WorkDir: f.workDir}
}

// emitJSON renders a machine-readable result when -json is set.
func emitJSON(f flags, r *report.Result) {
	if !f.jsonOut {
		return
	}
	data, err := report.Marshal(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock: encode json:", err)
		return
	}
	os.Stdout.Write(data)
}

func printIssues(errs []error) {
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "  -", classify(e), e)
	}
}

func runLock(f flags) int {
	res := &report.Result{Command: "lock", LockPath: f.lockPath}
	mf, errs, err := loadDeclarations(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		res.Issues = report.Classify([]error{err})
		emitJSON(f, res)
		return 2
	}
	if mf != nil {
		res.Subject = mf.ModulePath
	}
	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "golock: declarations rejected:")
		printIssues(errs)
		res.Issues = report.Classify(errs)
		emitJSON(f, res)
		return 1
	}

	modules, resErrs, err := resolver.Resolve(mf, f.resolverOptions())
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		res.Issues = report.Classify([]error{err})
		emitJSON(f, res)
		return 2
	}
	if len(resErrs) > 0 {
		// Atomicity: a failed resolution writes nothing, so a partial or
		// stale lock is never left behind by a failed run.
		fmt.Fprintln(os.Stderr, "golock: resolution incomplete; lock not written:")
		printIssues(resErrs)
		res.Issues = report.Classify(resErrs)
		emitJSON(f, res)
		return 1
	}

	generated := lockfile.Generate(mf, modules)
	data, err := lockfile.Marshal(generated)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock: marshal lock:", err)
		res.Issues = report.Classify([]error{err})
		emitJSON(f, res)
		return 2
	}

	var existing *model.LockFile
	if old, rerr := os.ReadFile(f.lockPath); rerr == nil {
		if lf, uerr := lockfile.Unmarshal(old); uerr == nil {
			existing = lf
		}
	}
	// Human-readable summaries go to stderr so stdout stays pure JSON
	// when -json is requested.
	summary := os.Stdout
	if f.jsonOut {
		summary = os.Stderr
	}
	if existing != nil && lockfile.Equal(existing, generated) {
		fmt.Fprintf(summary, "lock %s: already up to date (%d modules)\n", f.lockPath, len(modules))
	} else {
		if werr := lockfile.WriteAtomic(f.lockPath, data, 0o644); werr != nil {
			fmt.Fprintln(os.Stderr, "golock: write lock:", werr)
			res.Issues = report.Classify([]error{werr})
			emitJSON(f, res)
			return 2
		}
		fmt.Fprintf(summary, "lock %s: wrote %d modules\n", f.lockPath, len(modules))
	}
	res.OK = true
	res.ModulesWritten = len(modules)
	emitJSON(f, res)
	return 0
}

// runAudit performs the read-only change audit shared by verify and audit:
// it classifies declaration drift (lock_stale), lock tampering
// (lock_tampered) and cache content mismatches (missing_module /
// hash_mismatch) without ever modifying the lock or the cache.
func runAudit(f flags, command string) int {
	res := &report.Result{Command: command, LockPath: f.lockPath}
	mf, errs, err := loadDeclarations(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		res.Issues = report.Classify([]error{err})
		emitJSON(f, res)
		return 2
	}
	if mf != nil {
		res.Subject = mf.ModulePath
	}
	if len(errs) > 0 {
		fmt.Fprintln(os.Stderr, "golock: declarations rejected:")
		printIssues(errs)
		res.Issues = report.Classify(errs)
		emitJSON(f, res)
		return 1
	}

	vres, verrs, err := verify.RunManifest(mf, f.lockPath, f.resolverOptions())
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		res.Issues = report.Classify([]error{err})
		emitJSON(f, res)
		return 2
	}
	if vres != nil && vres.Report != nil && !f.jsonOut {
		printReport(vres.Report)
	}
	if vres != nil {
		res.Modules = report.ModulesFromReport(vres.Report)
	}
	res.Issues = report.Classify(verrs)
	if len(verrs) == 0 {
		res.OK = true
		emitJSON(f, res)
		if f.jsonOut {
			fmt.Fprintln(os.Stderr, "VERIFY OK: lock matches manifest and local cache")
		} else {
			fmt.Println("VERIFY OK: lock matches manifest and local cache")
		}
		return 0
	}
	emitJSON(f, res)
	fmt.Fprintln(os.Stderr, "VERIFY FAILED:")
	printIssues(verrs)
	return 1
}

func classify(e error) string {
	switch e.(type) {
	case *model.RejectError:
		return "[manifest]"
	case *model.WorkspaceError:
		return "[workspace]"
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
		case "-workspace", "-w":
			v, err := next()
			if err != nil {
				return err
			}
			f.workspace = v
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
		case "-json":
			f.jsonOut = true
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `golock - offline reproducible module locks

  golock lock   [-mod go.mod | -workspace golock.work] [-lock go.lock] [-cache DIR] [-json]
  golock verify [-mod go.mod | -workspace golock.work] [-lock go.lock] [-cache DIR] [-json]
  golock audit  [-mod go.mod | -workspace golock.work] [-lock go.lock] [-cache DIR] [-json]

Flags:
  -mod PATH        manifest file (default go.mod)
  -workspace PATH  workspace file describing several submodules
  -lock PATH       lock file (default go.lock)
  -cache DIR       module cache (default $GOMODCACHE)
  -workdir DIR     private scratch directory (default fresh temp dir)
  -json            emit a stable machine-readable JSON result
`)
}
