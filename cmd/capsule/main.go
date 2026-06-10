package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/z2z23n0/agent-capsule/internal/capsule"
	"github.com/z2z23n0/agent-capsule/internal/codex"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "capsule:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return flag.ErrHelp
	}
	switch args[0] {
	case "export":
		return runExport(args[1:])
	case "inspect":
		return runInspect(args[1:])
	case "restore":
		return runRestore(args[1:])
	case "verify":
		return runVerify(args[1:])
	case "help", "-h", "--help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runExport(args []string) error {
	fs := flag.NewFlagSet("export", flag.ExitOnError)
	thread := fs.String("thread", "current", "thread id to export, or current")
	home := fs.String("home", "", "source CODEX_HOME (defaults to CODEX_HOME or ~/.codex)")
	out := fs.String("out", "", "output .capsule.zip path")
	unsafe := fs.Bool("unsafe-include-secrets", false, "allow export when secret scan finds high-confidence secrets")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := capsule.Export(capsule.ExportOptions{
		Home:                 *home,
		Thread:               *thread,
		Out:                  *out,
		UnsafeIncludeSecrets: *unsafe,
	})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func runInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: capsule inspect <file.capsule.zip>")
	}
	result, err := capsule.Inspect(fs.Arg(0))
	if err != nil {
		return err
	}
	return printJSON(result)
}

func runRestore(args []string) error {
	fs := flag.NewFlagSet("restore", flag.ExitOnError)
	target := fs.String("target", "codex", "restore target (only codex is supported in v0.1)")
	home := fs.String("home", "", "target CODEX_HOME (defaults to CODEX_HOME or ~/.codex)")
	targetCWD := fs.String("target-cwd", "", "target cwd for the restored Codex thread (defaults to current directory)")
	execute := fs.Bool("execute", false, "perform writes; without this restore is a dry-run")
	replace := fs.Bool("replace", false, "replace an existing target thread/session with the same id")
	capsulePath := ""
	parseArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		capsulePath = args[0]
		parseArgs = args[1:]
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if capsulePath == "" {
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: capsule restore <file.capsule.zip> [--target codex] [--target-cwd .] [--execute]")
		}
		capsulePath = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return fmt.Errorf("usage: capsule restore <file.capsule.zip> [--target codex] [--target-cwd .] [--execute]")
	}
	if *target != "codex" {
		return fmt.Errorf("unsupported target %q: v0.1 only supports codex", *target)
	}
	result, err := capsule.Restore(capsulePath, codex.RestoreOptions{
		Home:      *home,
		TargetCWD: *targetCWD,
		Execute:   *execute,
		Replace:   *replace,
	})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	home := fs.String("home", "", "target CODEX_HOME (defaults to CODEX_HOME or ~/.codex)")
	thread := fs.String("thread", "", "thread id to verify")
	targetCWD := fs.String("target-cwd", "", "expected restored cwd")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *thread == "" {
		return fmt.Errorf("missing --thread")
	}
	result, err := capsule.Verify(*home, *thread, *targetCWD)
	if err != nil {
		return err
	}
	return printJSON(result)
}

func printJSON(value any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(value)
}

func usage() {
	fmt.Fprintln(os.Stderr, `capsule exports and restores local Codex session capsules.

Usage:
  capsule export --thread current --out session.capsule.zip
  capsule inspect session.capsule.zip
  capsule restore session.capsule.zip --target codex --target-cwd . --execute
  capsule verify --home ~/.codex --thread <thread-id> --target-cwd .`)
}
