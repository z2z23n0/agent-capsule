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
	case "import", "restore":
		return runImport(args[1:])
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
	out := fs.String("out", "", "fallback/output .capsule.zip path")
	name := fs.String("name", "", "capsule file name when --out is omitted")
	format := fs.String("format", "link", "export format: link or zip")
	service := fs.String("service", "official", "link service: official, worker, or s3")
	endpoint := fs.String("endpoint", "", "override worker endpoint for --service official/worker")
	token := fs.String("token", "", "worker bearer token (defaults to CAPSULE_WORKER_TOKEN)")
	unsafe := fs.Bool("unsafe-include-secrets", false, "allow export when secret scan finds high-confidence secrets")
	s3Endpoint := fs.String("s3-endpoint", "", "S3-compatible endpoint")
	s3Bucket := fs.String("s3-bucket", "", "S3/R2 bucket")
	s3Prefix := fs.String("s3-prefix", "", "S3/R2 key prefix")
	s3AccessKey := fs.String("s3-access-key-id", "", "S3/R2 access key id")
	s3SecretKey := fs.String("s3-secret-access-key", "", "S3/R2 secret access key")
	s3Region := fs.String("s3-region", "", "S3/R2 region (defaults to auto)")
	s3PublicBase := fs.String("s3-public-base-url", "", "public or presigned base URL for S3/R2 objects")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := capsule.Share(capsule.ShareOptions{
		Home:                 *home,
		Thread:               *thread,
		Out:                  *out,
		Name:                 *name,
		Format:               *format,
		Service:              *service,
		Endpoint:             *endpoint,
		Token:                *token,
		UnsafeIncludeSecrets: *unsafe,
		S3: capsule.S3Options{
			Endpoint:        *s3Endpoint,
			Bucket:          *s3Bucket,
			Prefix:          *s3Prefix,
			AccessKeyID:     *s3AccessKey,
			SecretAccessKey: *s3SecretKey,
			Region:          *s3Region,
			PublicBaseURL:   *s3PublicBase,
		},
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

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	target := fs.String("target", "codex", "import target (only codex is supported in v0.1)")
	home := fs.String("home", "", "target CODEX_HOME (defaults to CODEX_HOME or ~/.codex)")
	targetCWD := fs.String("target-cwd", "", "target cwd for the imported Codex thread (defaults to current directory)")
	execute := fs.Bool("execute", false, "write the imported thread into local Codex history")
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
			return fmt.Errorf("usage: capsule import <file.capsule.zip> [--target codex] [--target-cwd .] [--execute]")
		}
		capsulePath = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return fmt.Errorf("usage: capsule import <file.capsule.zip> [--target codex] [--target-cwd .] [--execute]")
	}
	if *target != "codex" {
		return fmt.Errorf("unsupported target %q: only codex is supported", *target)
	}
	result, err := capsule.RestoreAny(capsulePath, codex.RestoreOptions{
		Home:      *home,
		TargetCWD: *targetCWD,
		Execute:   *execute,
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
	targetCWD := fs.String("target-cwd", "", "expected imported cwd")
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
	fmt.Fprintln(os.Stderr, `capsule exports and imports local Codex session capsules.

Usage:
  capsule export --thread current
  capsule export --thread current --format zip --name "handoff topic"
  capsule export --thread current --service worker --endpoint https://example.workers.dev
  capsule export --thread current --service s3 --s3-endpoint https://<account>.r2.cloudflarestorage.com --s3-bucket agent-capsule --s3-public-base-url https://pub.example/capsules
  capsule inspect session.capsule.zip
  capsule import session.capsule.zip --target codex --target-cwd . --execute
  capsule import "https://example.workers.dev/s/share-id#k=..." --target codex --target-cwd . --execute
  capsule verify --home ~/.codex --thread <thread-id> --target-cwd .`)
}
