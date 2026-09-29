// Command golock locks Go module dependencies and verifies them offline.
//
// Usage:
//
//	golock lock   [-C dir] [--cache dir]
//	golock verify [-C dir] [--cache dir] [-v]
//
// Exit codes:
//
//	0 success / lock up to date
//	1 verification or locking failed (classified details are printed)
//	2 bad command-line usage
//
// golock never fetches from the network: every dependency must already be
// present in the module cache given by --cache or $GOMODCACHE.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/xingjianluo/go-module-lock-verifier-go/internal/verify"
)

const usageText = `golock - offline dependency locking and verification

Commands:
  lock     Resolve dependencies and (re)write ./go.lock atomically.
  verify   Check declarations, lock integrity and cached dependency digests.

Flags (both commands):
  -C dir     Directory containing go.mod (default: current directory)
  --cache dir  Read module cache from dir instead of $GOMODCACHE
  -v         Verbose: print each checked module and diff rows (verify only)
`

func main() {
	code := run(context.Background(), os.Args[1:])
	os.Exit(code)
}

func run(ctx context.Context, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usageText)
		return 2
	}
	cmd := args[0]
	rest := args[1:]

	fs := flag.NewFlagSet(cmd, flag.ContinueOnError)
	moduleDir := fs.String("C", ".", "directory containing go.mod")
	cacheDir := fs.String("cache", "", "module cache root (defaults to $GOMODCACHE)")
	verbose := fs.Bool("v", false, "verbose output")
	fs.Usage = func() { fmt.Fprint(os.Stderr, usageText) }
	if err := fs.Parse(rest); err != nil {
		return 2
	}

	abs, err := resolveDir(*moduleDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 2
	}
	opts := verify.Options{ModuleDir: abs, CacheRoot: *cacheDir}
	v := verify.New()

	switch cmd {
	case "lock":
		return doLock(ctx, v, opts)
	case "verify":
		return doVerify(ctx, v, opts, *verbose)
	case "-h", "--help", "help":
		fmt.Print(usageText)
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usageText)
		return 2
	}
}

func doLock(ctx context.Context, v *verify.Verifier, opts verify.Options) int {
	lock, desired, changed, err := v.EnsureLock(ctx, opts)
	if err != nil {
		printFailure(err)
		return 1
	}
	if changed {
		fmt.Printf("locked %d dependencies -> %s\n", len(lock.Entries), opts.EffectiveLockPath())
	} else {
		fmt.Printf("go.lock already up to date (%d dependencies)\n", len(lock.Entries))
	}
	if len(desired) == 0 {
		fmt.Println("(no dependencies)")
	}
	return 0
}

func doVerify(ctx context.Context, v *verify.Verifier, opts verify.Options, verbose bool) int {
	rep, err := v.Verify(ctx, opts)
	if err != nil {
		printFailure(err)
		if rep != nil && verbose {
			printReport(rep)
		}
		return 1
	}
	fmt.Printf("OK: %d dependencies verified offline (manifest %s)\n",
		rep.Checked, short(rep.ManifestHash))
	if verbose {
		printReport(rep)
	}
	return 0
}

func printFailure(err error) {
	fmt.Fprintf(os.Stderr, "FAILED: %s\n", strings.TrimSpace(err.Error()))
	if fl, ok := verify.AsFailureList(err); ok {
		for _, f := range fl {
			fmt.Fprintf(os.Stderr, "  [%s] %s\n", f.Code, firstLine(f.Detail))
			for _, d := range f.Diffs {
				fmt.Fprintf(os.Stderr,
					"      %-40s %-15s expected=%q actual=%q via %s\n",
					d.Path, d.Field, d.Expected, d.Actual,
					strings.Join(d.IntroducedBy, " -> "))
			}
		}
	}
}

func printReport(rep *verify.Report) {
	fmt.Printf("main module : %s\n", rep.MainModule)
	fmt.Printf("manifest    : %s\n", rep.ManifestHash)
	fmt.Printf("lock present: %v\n", rep.LockPresent)
	fmt.Printf("checked     : %d\n", rep.Checked)
	for _, d := range rep.Diffs {
		fmt.Printf("  diff %s %s: expected=%q actual=%q via %s\n",
			d.Path, d.Field, d.Expected, d.Actual,
			strings.Join(d.IntroducedBy, " -> "))
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func short(hash string) string {
	if len(hash) > 19 {
		return hash[:19]
	}
	return hash
}

func resolveDir(dir string) (string, error) {
	abs := dir
	if abs == "" || abs == "." {
		var err error
		abs, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	return abs, nil
}
