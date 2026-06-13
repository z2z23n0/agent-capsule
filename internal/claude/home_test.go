package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/z2z23n0/agent-capsule/internal/neutral"
)

func TestRestoreSessionReturnsFallbackResultWhenDirectWriteFails(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	targetCWD := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(targetCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	projectDirPath := filepath.Join(home, ProjectsDir, ProjectDirName(targetCWD))
	if err := os.MkdirAll(filepath.Dir(projectDirPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(projectDirPath, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := RestoreSession(RestoreInput{
		SourceAgent: "codex",
		SessionID:   "source",
		Title:       "handoff",
		Neutral: neutral.Transcript{
			Schema:      neutral.Schema,
			SourceAgent: "codex",
			SourceID:    "source",
			Title:       "handoff",
			Entries: []neutral.Entry{
				{Kind: "message", Role: "user", Text: "continue this"},
			},
		},
	}, RestoreOptions{Home: home, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatalf("RestoreSession returned error: %v", err)
	}
	if result.Status != "needs_cli_fallback" {
		t.Fatalf("status = %q, want needs_cli_fallback", result.Status)
	}
	if result.Error == "" {
		t.Fatal("expected error details")
	}
	if !strings.Contains(result.FallbackCommand, "claude --session-id "+result.SessionID) {
		t.Fatalf("fallback command = %q", result.FallbackCommand)
	}
}
