package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z2z23n0/agent-capsule/internal/capsule"
)

func TestShareCommandRemoved(t *testing.T) {
	err := run([]string{"share"})
	if err == nil || !strings.Contains(err.Error(), `unknown command "share"`) {
		t.Fatalf("expected share to be removed, got %v", err)
	}
}

func TestHandoffCommandRemoved(t *testing.T) {
	err := run([]string{"handoff"})
	if err == nil || !strings.Contains(err.Error(), `unknown command "handoff"`) {
		t.Fatalf("expected handoff to be removed, got %v", err)
	}
}

func TestProfileHelpCommand(t *testing.T) {
	if err := run([]string{"profile", "help"}); err != nil {
		t.Fatal(err)
	}
}

func TestImportCommandOpensRestoredCodexThread(t *testing.T) {
	sourceHome, threadID := createFakeCodexHome(t)
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := capsule.Export(capsule.ExportOptions{Home: sourceHome, Thread: threadID, Out: out}); err != nil {
		t.Fatal(err)
	}
	targetHome := t.TempDir()
	targetCWD := t.TempDir()
	var opened []string
	restoreLauncher := stubCodexThreadLauncher(t, &opened)
	defer restoreLauncher()

	if err := run([]string{"import", out, "--target", "codex", "--home", targetHome, "--target-cwd", targetCWD, "--execute"}); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 1 {
		t.Fatalf("opened threads = %v, want exactly one", opened)
	}
	if opened[0] == "" || opened[0] == threadID {
		t.Fatalf("opened thread id = %q, want new imported thread", opened[0])
	}
}

func TestImportCommandDoesNotOpenDryRun(t *testing.T) {
	sourceHome, threadID := createFakeCodexHome(t)
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := capsule.Export(capsule.ExportOptions{Home: sourceHome, Thread: threadID, Out: out}); err != nil {
		t.Fatal(err)
	}
	var opened []string
	restoreLauncher := stubCodexThreadLauncher(t, &opened)
	defer restoreLauncher()

	if err := run([]string{"import", out, "--target", "codex", "--home", t.TempDir(), "--target-cwd", t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if len(opened) != 0 {
		t.Fatalf("opened dry-run threads = %v, want none", opened)
	}
}

func stubCodexThreadLauncher(t *testing.T, opened *[]string) func() {
	t.Helper()
	previous := launchCodexThread
	launchCodexThread = func(threadID string) error {
		*opened = append(*opened, threadID)
		return nil
	}
	return func() {
		launchCodexThread = previous
	}
}

func createFakeCodexHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	threadID := "019e0000-0000-7000-8000-000000000123"
	sessionDir := filepath.Join(home, "sessions", "2026", "06", "11")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "rollout-2026-06-11T00-00-00-"+threadID+".jsonl")
	lines := []string{
		`{"timestamp":"2026-06-11T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","timestamp":"2026-06-11T00:00:00Z","cwd":"/source/project","cli_version":"0.138.0-alpha.7","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-11T00:00:01Z","type":"turn_context","payload":{"cwd":"/source/project","approval_policy":"never"}}`,
		`{"timestamp":"2026-06-11T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"share this session"}]}}`,
		`{"timestamp":"2026-06-11T00:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"ready to restore"}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(home, "session_index.jsonl"), map[string]any{
		"id":          threadID,
		"thread_name": "Test Session",
		"updated_at":  "2026-06-11T00:00:03Z",
	})
	return home, threadID
}

func writeJSONL(t *testing.T, path string, value any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
