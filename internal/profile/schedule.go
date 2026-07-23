package profile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

func ScheduleImport(opts ScheduleOptions) (*ScheduleResult, error) {
	if opts.Submit && runtime.GOOS != "darwin" {
		return nil, fmt.Errorf("profile schedule-import is supported only on macOS")
	}
	manifest, err := readManifest(opts.BundleDir)
	if err != nil {
		return nil, err
	}
	home := opts.Home
	if home == "" {
		home = manifest.TargetHome
	}
	home, err = resolveHome(home)
	if err != nil {
		return nil, err
	}
	cliPath := opts.CLIPath
	if cliPath == "" {
		cliPath, err = os.Executable()
		if err != nil {
			return nil, err
		}
	}
	cliPath, err = filepath.Abs(cliPath)
	if err != nil {
		return nil, err
	}
	staging := filepath.Join(home, "profile-migrations", manifest.ID)
	if filepath.Clean(opts.BundleDir) != filepath.Clean(staging) {
		if _, err := Fetch(FetchOptions{Source: opts.BundleDir, Out: staging, IncludeGitBundles: true}); err != nil {
			return nil, err
		}
	}
	label := "com.agent-capsule.profile-import." + strings.ReplaceAll(manifest.ID, "_", "-")
	runner := filepath.Join(staging, "run-import.sh")
	statusPath := filepath.Join(staging, "import-status.json")
	logPath := filepath.Join(staging, "import.log")
	runnerText := fmt.Sprintf(`#!/bin/bash
set -euo pipefail
PLIST_PATH=%s
cleanup() { /bin/rm -f "$PLIST_PATH"; }
trap cleanup EXIT
sleep 5
/usr/bin/osascript -e 'tell application "ChatGPT" to quit' >/dev/null 2>&1 || true
attempt=0
while [ "$attempt" -lt 30 ]; do
  if ! /usr/bin/pgrep -x ChatGPT >/dev/null 2>&1; then break; fi
  sleep 1
  attempt=$((attempt + 1))
done
if /usr/bin/pgrep -x ChatGPT >/dev/null 2>&1; then
  /usr/bin/pkill -TERM -x ChatGPT || true
  sleep 3
fi
if /usr/bin/pgrep -x ChatGPT >/dev/null 2>&1; then
  printf '{"status":"failed","error":"Codex App did not stop"}\n' > %s
  exit 1
fi
if %s profile import %s --home %s --execute; then
  if %s profile verify %s --home %s; then
    printf '{"status":"ok"}\n' > %s
    /usr/bin/open -a ChatGPT
    exit 0
  fi
fi
printf '{"status":"failed","error":"import or verification failed","log":%s}\n' > %s
/usr/bin/open -a ChatGPT
exit 1
`, quoteShell(plistPathFor(home, label)), quoteShell(statusPath), quoteShell(cliPath), quoteShell(staging), quoteShell(home), quoteShell(cliPath), quoteShell(staging), quoteShell(home), quoteShell(statusPath), strconv.Quote(logPath), quoteShell(statusPath))
	if err := os.WriteFile(runner, []byte(runnerText), 0o700); err != nil {
		return nil, err
	}
	launchDir := filepath.Join(filepath.Dir(home), "Library", "LaunchAgents")
	if err := os.MkdirAll(launchDir, 0o755); err != nil {
		return nil, err
	}
	plistPath := filepath.Join(launchDir, label+".plist")
	plist := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>%s</string>
  <key>ProgramArguments</key>
  <array><string>/bin/bash</string><string>%s</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><false/>
  <key>ProcessType</key><string>Background</string>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict>
</plist>
`, xmlEscape(label), xmlEscape(runner), xmlEscape(logPath), xmlEscape(logPath))
	if err := os.WriteFile(plistPath, []byte(plist), 0o600); err != nil {
		return nil, err
	}
	result := &ScheduleResult{Status: "prepared", Label: label, StagingDir: staging, PlistPath: plistPath, RunnerPath: runner, StatusPath: statusPath, LogPath: logPath}
	if !opts.Submit {
		return result, nil
	}
	uid := os.Getuid()
	domain := fmt.Sprintf("gui/%d", uid)
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	if output, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput(); err != nil {
		return nil, fmt.Errorf("launchctl bootstrap: %w: %s", err, output)
	}
	result.Status = "scheduled"
	return result, nil
}

func plistPathFor(home, label string) string {
	return filepath.Join(filepath.Dir(home), "Library", "LaunchAgents", label+".plist")
}

func Unschedule(opts UnscheduleOptions) (*UnscheduleResult, error) {
	manifest, err := readManifest(opts.BundleDir)
	if err != nil {
		return nil, err
	}
	label := "com.agent-capsule.profile-import." + strings.ReplaceAll(manifest.ID, "_", "-")
	home := opts.Home
	if home == "" {
		home = manifest.TargetHome
	}
	home, err = resolveHome(home)
	if err != nil {
		return nil, err
	}
	plistPath := filepath.Join(filepath.Dir(home), "Library", "LaunchAgents", label+".plist")
	result := &UnscheduleResult{Status: "planned", Label: label, PlistPath: plistPath}
	if !opts.Submit {
		return result, nil
	}
	domain := fmt.Sprintf("gui/%d", os.Getuid())
	_ = exec.Command("launchctl", "bootout", domain+"/"+label).Run()
	if err := os.Remove(plistPath); err != nil && !isNotExist(err) {
		return nil, err
	}
	result.Status = "ok"
	return result, nil
}

func xmlEscape(value string) string {
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return replacer.Replace(value)
}
