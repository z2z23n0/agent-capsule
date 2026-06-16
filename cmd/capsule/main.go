package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/z2z23n0/agent-capsule/internal/capsule"
	"github.com/z2z23n0/agent-capsule/internal/claude"
	"github.com/z2z23n0/agent-capsule/internal/codex"
)

var launchCodexThread = defaultLaunchCodexThread

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
	source := fs.String("source", "codex", "source agent: codex or claude")
	thread := fs.String("thread", "current", "thread id to export, or current")
	home := fs.String("home", "", "source agent home (defaults to CODEX_HOME/~/.codex or CLAUDE_CONFIG_DIR/~/.claude)")
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
		SourceAgent:          *source,
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
	target := fs.String("target", "codex", "import target: codex or claude")
	home := fs.String("home", "", "target agent home (defaults to CODEX_HOME/~/.codex or CLAUDE_CONFIG_DIR/~/.claude)")
	targetCWD := fs.String("target-cwd", "", "target cwd for the imported session/thread (defaults to current directory)")
	execute := fs.Bool("execute", false, "write the imported session/thread into local agent history")
	allowModelCall := fs.Bool("allow-model-call", false, "allow CLI fallback paths that may call a model")
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
	result, err := capsule.ImportAny(capsulePath, capsule.ImportOptions{
		Target:         *target,
		Home:           *home,
		TargetCWD:      *targetCWD,
		Execute:        *execute,
		AllowModelCall: *allowModelCall,
	})
	if err != nil {
		return err
	}
	maybeLaunchImportedCodexThread(*target, *execute, result)
	return printJSON(result)
}

func runVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	target := fs.String("target", "codex", "target agent: codex or claude")
	home := fs.String("home", "", "target agent home")
	thread := fs.String("thread", "", "thread/session id to verify")
	targetCWD := fs.String("target-cwd", "", "expected imported cwd")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *thread == "" {
		return fmt.Errorf("missing --thread")
	}
	var result any
	var err error
	switch *target {
	case "codex":
		result, err = capsule.Verify(*home, *thread, *targetCWD)
	case "claude":
		result, err = claude.VerifySession(*home, *thread, *targetCWD)
	default:
		return fmt.Errorf("unsupported target %q", *target)
	}
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

func maybeLaunchImportedCodexThread(target string, execute bool, result any) {
	if !execute || !strings.EqualFold(strings.TrimSpace(target), "codex") {
		return
	}
	restored, ok := result.(*codex.RestoreResult)
	if !ok || restored.ThreadID == "" || restored.DryRun || restored.Status != "ok" {
		return
	}
	if err := launchCodexThread(restored.ThreadID); err != nil {
		fmt.Fprintf(os.Stderr, "capsule: warning: imported thread %s but failed to open Codex App: %v\n", restored.ThreadID, err)
	}
}

func defaultLaunchCodexThread(threadID string) error {
	if runtime.GOOS != "darwin" {
		return nil
	}
	return exec.Command("open", "codex://threads/"+threadID).Run()
}

func usage() {
	fmt.Fprintln(os.Stderr, `capsule exports and imports local coding-agent sessions.

Usage:
  capsule export --source codex --thread current
  capsule export --source claude --thread current --format zip
  capsule export --thread current --format zip --name "handoff topic"
  capsule export --thread current --service worker --endpoint https://example.workers.dev
  capsule export --thread current --service s3 --s3-endpoint https://<account>.r2.cloudflarestorage.com --s3-bucket agent-capsule --s3-public-base-url https://pub.example/capsules
  capsule inspect session.capsule.zip
  capsule import session.capsule.zip --target codex --target-cwd . --execute
  capsule import session.capsule.zip --target claude --target-cwd . --execute
  capsule import "https://example.workers.dev/s/share-id#k=..." --target codex --target-cwd . --execute
  capsule verify --target codex --home ~/.codex --thread <thread-id> --target-cwd .
  capsule verify --target claude --home ~/.claude --thread <session-id> --target-cwd .`)
}
