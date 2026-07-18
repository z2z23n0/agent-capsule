package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/z2z23n0/agent-capsule/internal/profile"
)

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }
func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func runProfile(args []string) error {
	if len(args) == 0 {
		profileUsage()
		return flag.ErrHelp
	}
	switch args[0] {
	case "discover":
		return runProfileDiscover(args[1:])
	case "export":
		return runProfileExport(args[1:])
	case "fetch":
		return runProfileFetch(args[1:])
	case "clone":
		return runProfileClone(args[1:])
	case "import":
		return runProfileImport(args[1:])
	case "verify":
		return runProfileVerify(args[1:])
	case "schedule-import":
		return runProfileSchedule(args[1:])
	case "unschedule":
		return runProfileUnschedule(args[1:])
	case "serve":
		return runProfileServe(args[1:])
	case "help", "-h", "--help":
		profileUsage()
		return nil
	default:
		return fmt.Errorf("unknown profile command %q", args[0])
	}
}

func runProfileDiscover(args []string) error {
	fs := flag.NewFlagSet("profile discover", flag.ContinueOnError)
	home := fs.String("home", "", "source Codex home")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := profile.Discover(profile.DiscoverOptions{Home: *home})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func runProfileExport(args []string) error {
	fs := flag.NewFlagSet("profile export", flag.ContinueOnError)
	home := fs.String("home", "", "source Codex home")
	targetHome := fs.String("target-home", "", "target Codex home")
	targetWorkspace := fs.String("target-workspace", "", "target project workspace")
	out := fs.String("out", "", "output profile bundle directory")
	unsafe := fs.Bool("unsafe-include-secrets", false, "allow high-confidence secrets in allowlisted profile files")
	gitBundles := fs.Bool("git-bundle-fallback", false, "prepare committed Git history as a private-clone fallback")
	var projects stringList
	fs.Var(&projects, "project", "project root to migrate; repeat for each project")
	if err := fs.Parse(args); err != nil {
		return err
	}
	result, err := profile.Export(profile.ExportOptions{Home: *home, TargetHome: *targetHome, TargetWorkspace: *targetWorkspace, Projects: projects, Out: *out, UnsafeIncludeSecrets: *unsafe, GitBundleFallback: *gitBundles})
	if err != nil {
		return err
	}
	return printJSON(result)
}

func positionalFirst(name string, args []string, configure func(*flag.FlagSet), run func(string, *flag.FlagSet) error) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	configure(fs)
	positional := ""
	parseArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		positional = args[0]
		parseArgs = args[1:]
	}
	if err := fs.Parse(parseArgs); err != nil {
		return err
	}
	if positional == "" && fs.NArg() == 1 {
		positional = fs.Arg(0)
	} else if fs.NArg() != 0 {
		return fmt.Errorf("usage: capsule %s <source> [flags]", name)
	}
	if positional == "" {
		return fmt.Errorf("usage: capsule %s <source> [flags]", name)
	}
	return run(positional, fs)
}

func runProfileFetch(args []string) error {
	var out *string
	var includeGitBundles *bool
	return positionalFirst("profile fetch", args, func(fs *flag.FlagSet) {
		out = fs.String("out", "", "target profile bundle directory")
		includeGitBundles = fs.Bool("include-git-bundles", false, "fetch private-clone fallback bundles")
	}, func(source string, _ *flag.FlagSet) error {
		result, err := profile.Fetch(profile.FetchOptions{Source: source, Out: *out, IncludeGitBundles: *includeGitBundles})
		if err != nil {
			return err
		}
		return printJSON(result)
	})
}

func runProfileClone(args []string) error {
	var execute *bool
	return positionalFirst("profile clone", args, func(fs *flag.FlagSet) {
		execute = fs.Bool("execute", false, "clone project repositories and checkout exported commits")
	}, func(bundle string, _ *flag.FlagSet) error {
		result, err := profile.CloneProjects(profile.CloneOptions{BundleDir: bundle, Execute: *execute})
		if err != nil {
			return err
		}
		return printJSON(result)
	})
}

func runProfileImport(args []string) error {
	var home *string
	var execute *bool
	return positionalFirst("profile import", args, func(fs *flag.FlagSet) {
		home = fs.String("home", "", "target Codex home")
		execute = fs.Bool("execute", false, "apply the controlled overwrite")
	}, func(bundle string, _ *flag.FlagSet) error {
		result, err := profile.Import(profile.ImportOptions{BundleDir: bundle, Home: *home, Execute: *execute, RequireStopped: *execute})
		if err != nil {
			return err
		}
		return printJSON(result)
	})
}

func runProfileVerify(args []string) error {
	var home *string
	return positionalFirst("profile verify", args, func(fs *flag.FlagSet) {
		home = fs.String("home", "", "target Codex home")
	}, func(bundle string, _ *flag.FlagSet) error {
		result, err := profile.Verify(profile.VerifyOptions{BundleDir: bundle, Home: *home})
		if err != nil {
			return err
		}
		if err := printJSON(result); err != nil {
			return err
		}
		if result.Status != "ok" {
			return fmt.Errorf("profile verification failed")
		}
		return nil
	})
}

func runProfileSchedule(args []string) error {
	var home, cli *string
	var execute *bool
	return positionalFirst("profile schedule-import", args, func(fs *flag.FlagSet) {
		home = fs.String("home", "", "target Codex home")
		cli = fs.String("cli", "", "capsule executable path")
		execute = fs.Bool("execute", false, "bootstrap the one-shot LaunchAgent")
	}, func(bundle string, _ *flag.FlagSet) error {
		result, err := profile.ScheduleImport(profile.ScheduleOptions{BundleDir: bundle, Home: *home, CLIPath: *cli, Submit: *execute})
		if err != nil {
			return err
		}
		return printJSON(result)
	})
}

func runProfileUnschedule(args []string) error {
	var home *string
	var execute *bool
	return positionalFirst("profile unschedule", args, func(fs *flag.FlagSet) {
		home = fs.String("home", "", "target Codex home")
		execute = fs.Bool("execute", false, "unload and remove the LaunchAgent plist")
	}, func(bundle string, _ *flag.FlagSet) error {
		result, err := profile.Unschedule(profile.UnscheduleOptions{BundleDir: bundle, Home: *home, Submit: *execute})
		if err != nil {
			return err
		}
		return printJSON(result)
	})
}

func runProfileServe(args []string) error {
	var listen *string
	return positionalFirst("profile serve", args, func(fs *flag.FlagSet) {
		listen = fs.String("listen", ":8765", "listen address")
	}, func(bundle string, _ *flag.FlagSet) error {
		server, err := profile.NewServer(bundle, *listen)
		if err != nil {
			return err
		}
		if err := printJSON(map[string]any{"status": "serving", "bundle_dir": filepath.Clean(bundle), "urls": server.URLs, "pid": os.Getpid()}); err != nil {
			return err
		}
		return server.Serve()
	})
}

func profileUsage() {
	fmt.Fprintln(os.Stderr, `Codex profile migration commands:
  capsule profile discover [--home ~/.codex]
  capsule profile export --project <path>... --target-home /Users/<user>/.codex --target-workspace /Users/<user>/workspace --out <dir>
  capsule profile serve <dir> --listen :8765
  capsule profile fetch <dir-or-http-url> --out ~/.codex/profile-migrations/<id>
  capsule profile clone <dir> [--execute]
  capsule profile import <dir> --home ~/.codex [--execute]
  capsule profile schedule-import <dir> --home ~/.codex [--execute]
  capsule profile verify <dir> --home ~/.codex
  capsule profile unschedule <dir> [--execute]`)
}
