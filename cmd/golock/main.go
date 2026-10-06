// Command golock generates and verifies reproducible module locks offline,
// for a single module or a multi-module workspace.
//
// Usage:
//
//	golock lock   [-mod go.mod | -work golock.work] [-lock go.lock] [-cache GOMODCACHE] [-json]
//	golock verify [-mod go.mod] [-lock go.lock] [-cache GOMODCACHE] [-json]
//	golock audit  [-mod go.mod | -work golock.work] [-lock go.lock] [-cache GOMODCACHE] [-json]
//
// All subcommands are read-only against the module cache. lock writes the
// lock file atomically and only when the run fully succeeded; verify and
// audit never write anything. -json emits a stable, machine-readable report
// on stdout (see internal/report).
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/audit"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/lockfile"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/manifest"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/model"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/report"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/resolver"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/verify"
	"github.com/xingjianluo/go-module-lock-verifier-go/internal/workspace"
)

type flags struct {
	modPath  string
	workPath string
	lockPath string
	cache    string
	workDir  string
	json     bool
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
	f := flags{lockPath: "go.lock", cache: cache}
	if err := parseFlags(&f, args); err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		os.Exit(2)
	}
	if f.modPath == "" && f.workPath == "" {
		f.modPath = "go.mod"
	}
	if f.modPath != "" && f.workPath != "" {
		fmt.Fprintln(os.Stderr, "golock: -mod and -work are mutually exclusive")
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
	case "audit":
		code = runAudit(f)
	case "-h", "--help", "help":
		usage()
	default:
		usage()
		code = 2
	}
	os.Exit(code)
}

// declarations loads the effective manifest for lock: either a single go.mod
// or the merged workspace view. Classified problems come back as []error.
// merged is non-nil only in workspace mode and carries provenance.
func declarations(f flags) (mf *model.Manifest, merged *workspace.Merged, subject string, modules []string, errs []error, err error) {
	if f.workPath != "" {
		ws, loadErrs, lerr := workspace.Load(f.workPath)
		if lerr != nil {
			return nil, nil, "", nil, nil, lerr
		}
		subject = ws.Name
		for _, m := range ws.Modules {
			if m.Manifest != nil {
				modules = append(modules, m.Manifest.ModulePath)
			}
		}
		if len(loadErrs) > 0 {
			return nil, nil, subject, modules, loadErrs, nil
		}
		m, mergeErrs := workspace.Merge(ws)
		if len(mergeErrs) > 0 {
			return nil, nil, subject, modules, mergeErrs, nil
		}
		return m.Manifest, m, subject, modules, nil, nil
	}
	mf, rejects, perr := manifest.ParseFile(f.modPath)
	if perr != nil {
		return nil, nil, "", nil, nil, fmt.Errorf("read manifest: %w", perr)
	}
	if len(rejects) > 0 {
		return nil, nil, mf.ModulePath, nil, rejects, nil
	}
	return mf, nil, mf.ModulePath, nil, nil, nil
}

func runLock(f flags) int {
	mf, merged, subject, modules, declErrs, err := declarations(f)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		return 2
	}
	if len(declErrs) > 0 {
		return fail(f, "lock", subject, modules, 0, declErrs)
	}

	modules2, resErrs, err := resolver.Resolve(mf, resolver.Options{
		ModuleCache: f.cache, WorkDir: f.workDir,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		return 2
	}
	// Atomicity: a run with any resolution problem writes nothing, so a
	// partial selection can never land on disk as a残缺 lock.
	if len(resErrs) > 0 {
		return fail(f, "lock", subject, modules, 0, resErrs)
	}
	if merged != nil {
		// Record which submodule (not the abstract workspace) pulled each
		// dependency in.
		merged.Attribute(modules2)
	}

	generated := lockfile.Generate(mf, modules2)
	data, err := lockfile.Marshal(generated)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock: marshal lock:", err)
		return 2
	}

	var existing *model.LockFile
	if old, rerr := os.ReadFile(f.lockPath); rerr == nil {
		if lf, uerr := lockfile.Unmarshal(old); uerr == nil {
			existing = lf
		}
	}
	wrote := false
	if existing != nil && lockfile.Equal(existing, generated) {
		fmt.Printf("lock %s: already up to date (%d modules)\n", f.lockPath, len(modules2))
	} else {
		if werr := lockfile.WriteAtomic(f.lockPath, data); werr != nil {
			fmt.Fprintln(os.Stderr, "golock: write lock:", werr)
			return 2
		}
		wrote = true
		fmt.Printf("lock %s: wrote %d modules\n", f.lockPath, len(modules2))
	}
	_ = wrote
	return ok(f, "lock", subject, modules, len(modules2))
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
	locked := 0
	if res != nil && res.Lock != nil {
		locked = len(res.Lock.Modules)
	}
	if len(errs) > 0 {
		if res != nil && res.Report != nil && !f.json {
			printReport(res.Report)
		}
		return fail(f, "verify", f.modPath, nil, locked, errs)
	}
	if res != nil && res.Report != nil && !f.json {
		printReport(res.Report)
		fmt.Println("VERIFY OK: lock matches manifest and local cache")
	}
	return ok(f, "verify", f.modPath, nil, locked)
}

func runAudit(f flags) int {
	res, errs, err := audit.Run(audit.Options{
		ManifestPath:  f.modPath,
		WorkspacePath: f.workPath,
		LockPath:      f.lockPath,
		Resolver: resolver.Options{
			ModuleCache: f.cache, WorkDir: f.workDir,
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock:", err)
		return 2
	}
	locked := 0
	if res != nil && res.Lock != nil {
		locked = len(res.Lock.Modules)
	}
	subject := ""
	var modules []string
	if res != nil {
		subject, modules = res.Subject, res.Modules
	}
	if len(errs) > 0 {
		if res != nil && res.Report != nil && !f.json {
			printReport(res.Report)
		}
		return fail(f, "audit", subject, modules, locked, errs)
	}
	if res != nil && res.Report != nil && !f.json {
		printReport(res.Report)
		fmt.Println("AUDIT OK: lock matches declarations and local cache")
	}
	return ok(f, "audit", subject, modules, locked)
}

// ok emits the success result (JSON when requested) and returns exit code 0.
func ok(f flags, cmd, subject string, modules []string, locked int) int {
	if f.json {
		emit(&report.Report{
			SchemaVersion: report.SchemaVersion, Command: cmd, OK: true,
			Subject: subject, Modules: modules, LockedModules: locked,
		})
	}
	return 0
}

// fail prints classified findings (human or JSON form) and returns exit 1.
func fail(f flags, cmd, subject string, modules []string, locked int, errs []error) int {
	if f.json {
		emit(&report.Report{
			SchemaVersion: report.SchemaVersion, Command: cmd, OK: false,
			Subject: subject, Modules: modules, LockedModules: locked,
			Findings: report.FromErrors(errs),
		})
		return 1
	}
	fmt.Fprintf(os.Stderr, "%s FAILED:\n", cmd)
	for _, e := range errs {
		fmt.Fprintln(os.Stderr, "  -", classify(e), e)
	}
	return 1
}

func emit(r *report.Report) {
	data, err := report.Marshal(r)
	if err != nil {
		fmt.Fprintln(os.Stderr, "golock: encode report:", err)
		return
	}
	os.Stdout.Write(data)
}

func classify(e error) string {
	switch e.(type) {
	case *model.RejectError:
		return "[manifest]"
	case *model.LockError:
		return "[lock]"
	case *model.VerifyError:
		return "[cache]"
	case *model.WorkspaceError:
		return "[workspace]"
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
		case "-work":
			v, err := next()
			if err != nil {
				return err
			}
			f.workPath = v
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
			f.json = true
		default:
			return fmt.Errorf("unknown flag %q", a)
		}
	}
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `golock - offline reproducible module locks

  golock lock   [-mod go.mod | -work golock.work] [-lock go.lock] [-cache DIR] [-json]
  golock verify [-mod go.mod] [-lock go.lock] [-cache DIR] [-json]
  golock audit  [-mod go.mod | -work golock.work] [-lock go.lock] [-cache DIR] [-json]

Flags:
  -mod PATH     manifest file (default go.mod; mutually exclusive with -work)
  -work PATH    workspace file listing submodule dirs (see docs/workspace.md)
  -lock PATH    lock file (default go.lock)
  -cache DIR    module cache (default $GOMODCACHE)
  -workdir DIR  private scratch directory (default fresh temp dir)
  -json         emit a stable machine-readable report on stdout
`)
}
