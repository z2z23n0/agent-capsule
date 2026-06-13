package capsule

import (
	"archive/zip"
	"crypto/aes"
	"crypto/cipher"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	_ "modernc.org/sqlite"

	"github.com/z2z23n0/agent-capsule/internal/claude"
	"github.com/z2z23n0/agent-capsule/internal/codex"
)

const testThreadID = "019e0000-0000-7000-8000-000000000001"
const testClaudeSessionID = "801c0d56-31be-436e-81b5-b25efeca0562"

func TestExportCreatesStandardZipWithAgentReadme(t *testing.T) {
	home := createFakeCodexHome(t)
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	result, err := Export(ExportOptions{Home: home, Thread: testThreadID, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != out {
		t.Fatalf("path = %q", result.Path)
	}
	reader, err := zip.OpenReader(out)
	if err != nil {
		t.Fatalf("not a standard zip: %v", err)
	}
	defer reader.Close()
	names := map[string]bool{}
	for _, file := range reader.File {
		names[file.Name] = true
	}
	for _, name := range RequiredFiles {
		if !names[name] {
			t.Fatalf("missing %s", name)
		}
	}
	readme := readZipFile(t, out, "AGENT_README.md")
	if !strings.Contains(readme, "go install github.com/z2z23n0/agent-capsule/cmd/capsule@main") {
		t.Fatalf("AGENT_README.md does not include install command:\n%s", readme)
	}
	if !strings.Contains(readme, DefaultSkill) {
		t.Fatalf("AGENT_README.md does not include skill URL:\n%s", readme)
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(readZipFile(t, out, "manifest.json")), &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest.SkillURL != DefaultSkill {
		t.Fatalf("skill URL = %q", manifest.SkillURL)
	}
}

func TestExportUsesNameWhenOutIsOmitted(t *testing.T) {
	t.Chdir(t.TempDir())
	home := createFakeCodexHome(t)
	result, err := Export(ExportOptions{Home: home, Thread: testThreadID, Name: "Agent Capsule fork demo"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Path != "Agent-Capsule-fork-demo.capsule.zip" {
		t.Fatalf("path = %q", result.Path)
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatal(err)
	}
}

func TestDefaultOutputNameUsesTitleThenFirstUserText(t *testing.T) {
	if got := DefaultOutputName("", "Project kickoff", "first prompt", testThreadID); got != "Project-kickoff.capsule.zip" {
		t.Fatalf("title output = %q", got)
	}
	if got := DefaultOutputName("", testThreadID, "share this session", testThreadID); got != "share-this-session.capsule.zip" {
		t.Fatalf("first user output = %q", got)
	}
}

func TestClaudeExportAndNativeImport(t *testing.T) {
	sourceHome, sourceCWD := createFakeClaudeHome(t)
	out := filepath.Join(t.TempDir(), "claude.capsule.zip")
	exported, err := Export(ExportOptions{SourceAgent: AgentClaude, Home: sourceHome, Thread: testClaudeSessionID, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if exported.Source != AgentClaude || exported.ThreadID != testClaudeSessionID {
		t.Fatalf("unexpected export result: %+v", exported)
	}
	if readZipFile(t, out, "claude/session.jsonl") == "" {
		t.Fatal("missing Claude raw session")
	}
	var manifest Manifest
	if err := json.Unmarshal([]byte(readZipFile(t, out, "manifest.json")), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SourceAgent != AgentClaude || manifest.LosslessLevel == "" {
		t.Fatalf("manifest missing Claude metadata: %+v", manifest)
	}
	targetHome := t.TempDir()
	targetCWD := filepath.Join(t.TempDir(), "target-project")
	if err := os.MkdirAll(targetCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	importedAny, err := Import(out, ImportOptions{Target: AgentClaude, Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	imported, ok := importedAny.(*claude.RestoreResult)
	if !ok {
		t.Fatalf("import result type = %T", importedAny)
	}
	if imported.SessionID == testClaudeSessionID {
		t.Fatal("Claude import reused source session id")
	}
	verify, err := claude.VerifySession(targetHome, imported.SessionID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("Claude verify failed: %+v", verify)
	}
	sourceBytes, err := os.ReadFile(filepath.Join(sourceHome, "projects", claude.ProjectDirName(sourceCWD), testClaudeSessionID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(sourceBytes), testClaudeSessionID) {
		t.Fatal("source Claude session was unexpectedly modified")
	}
}

func TestClaudeLinkManifestDefaultsToClaudeImportTarget(t *testing.T) {
	manifest := buildLinkManifest(&ExportResult{
		Source:   AgentClaude,
		ThreadID: testClaudeSessionID,
		Title:    "Claude handoff",
	}, encryptedCapsule{
		Ciphertext: []byte("ciphertext"),
		Nonce:      make([]byte, 12),
		SHA256:     "sha256",
	}, "official")
	if !strings.Contains(manifest.Import.Command, "--target claude") {
		t.Fatalf("import command = %q", manifest.Import.Command)
	}
	if manifest.Import.Command != manifest.Import.ExecuteCommand {
		t.Fatalf("command mismatch: %q != %q", manifest.Import.Command, manifest.Import.ExecuteCommand)
	}
}

func TestCodexToClaudeLocalHandoffWritesNativeSessionAndSidecar(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	targetHome := t.TempDir()
	targetCWD := filepath.Join(t.TempDir(), "claude-target")
	if err := os.MkdirAll(targetCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Handoff(HandoffOptions{
		From:         AgentCodex,
		To:           AgentClaude,
		SourceHome:   sourceHome,
		TargetHome:   targetHome,
		SourceThread: testThreadID,
		TargetCWD:    targetCWD,
		Execute:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.TargetID == "" {
		t.Fatalf("unexpected handoff result: %+v", result)
	}
	verify, err := claude.VerifySession(targetHome, result.TargetID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("Claude verify failed: %+v", verify)
	}
	sessionPath := filepath.Join(targetHome, "projects", claude.ProjectDirName(targetCWD), result.TargetID+".jsonl")
	content, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "share this session") || !strings.Contains(string(content), "ready to restore") {
		t.Fatalf("handoff content missing Codex transcript:\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(targetHome, "agent-capsule-sources", result.TargetID, "source.jsonl")); err != nil {
		t.Fatalf("missing Claude sidecar: %v", err)
	}
}

func TestClaudeToCodexLocalHandoffWritesNativeThreadAndSidecar(t *testing.T) {
	sourceHome, _ := createFakeClaudeHome(t)
	targetHome := createEmptyCodexHome(t)
	targetCWD := filepath.Join(t.TempDir(), "codex-target")
	if err := os.MkdirAll(targetCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Handoff(HandoffOptions{
		From:         AgentClaude,
		To:           AgentCodex,
		SourceHome:   sourceHome,
		TargetHome:   targetHome,
		SourceThread: testClaudeSessionID,
		TargetCWD:    targetCWD,
		Execute:      true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.TargetID == "" {
		t.Fatalf("unexpected handoff result: %+v", result)
	}
	verify, err := Verify(targetHome, result.TargetID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("Codex verify failed: %+v", verify)
	}
	sessionPath := verifySessionPath(t, targetHome, result.TargetID)
	content, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "Claude asks for help") || !strings.Contains(string(content), "Claude answer") {
		t.Fatalf("handoff content missing Claude transcript:\n%s", content)
	}
	if _, err := os.Stat(filepath.Join(targetHome, "agent-capsule-sources", result.TargetID, "source.jsonl")); err != nil {
		t.Fatalf("missing Codex sidecar: %v", err)
	}
}

func TestRestoreDryRunAndExecute(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: sourceHome, Thread: testThreadID, Out: out}); err != nil {
		t.Fatal(err)
	}
	targetHome := createEmptyCodexHome(t)
	targetCWD := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(targetCWD, 0o755); err != nil {
		t.Fatal(err)
	}
	plan, err := Restore(out, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun {
		t.Fatal("expected dry-run")
	}
	if plan.SourceThreadID != testThreadID {
		t.Fatalf("source thread id = %q", plan.SourceThreadID)
	}
	if plan.ThreadID == testThreadID {
		t.Fatal("dry-run planned import with source thread id")
	}
	verify, err := Verify(targetHome, plan.ThreadID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status == "ok" {
		t.Fatal("dry-run wrote restore state")
	}
	result, err := Restore(out, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ThreadID == testThreadID {
		t.Fatal("import reused source thread id")
	}
	verify, err = Verify(targetHome, result.ThreadID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("verify failed: %+v", verify)
	}
	content, err := os.ReadFile(result.TargetSessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if summary := codex.SummarizeSession(content); summary.ID != result.ThreadID {
		t.Fatalf("session_meta id = %q, want %q", summary.ID, result.ThreadID)
	}
	indexEntry := readIndexEntry(t, targetHome, result.ThreadID)
	if got := indexEntry["thread_name"]; got != "[agent-capsule] Test Session" {
		t.Fatalf("imported index title = %q", got)
	}
	db, err := sql.Open("sqlite", filepath.Join(targetHome, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var title, firstUserMessage, preview string
	if err := db.QueryRow("select title, first_user_message, preview from threads where id = ?", result.ThreadID).Scan(&title, &firstUserMessage, &preview); err != nil {
		t.Fatal(err)
	}
	if title != "[agent-capsule] Test Session" {
		t.Fatalf("imported sqlite title = %q", title)
	}
	if firstUserMessage != "share this session" {
		t.Fatalf("first user message was not preserved: %q", firstUserMessage)
	}
	if preview != "Test Session preview" {
		t.Fatalf("preview was not preserved: %q", preview)
	}
}

func TestRestoreImportsSameCapsuleTwiceAsNewThreads(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: sourceHome, Thread: testThreadID, Out: out}); err != nil {
		t.Fatal(err)
	}
	targetHome := createEmptyCodexHome(t)
	targetCWD := t.TempDir()
	first, err := Restore(out, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Restore(out, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.ThreadID == second.ThreadID || first.ThreadID == testThreadID || second.ThreadID == testThreadID {
		t.Fatalf("imports did not allocate distinct fork ids: first=%s second=%s source=%s", first.ThreadID, second.ThreadID, testThreadID)
	}
	for _, threadID := range []string{first.ThreadID, second.ThreadID} {
		verify, err := Verify(targetHome, threadID, targetCWD)
		if err != nil {
			t.Fatal(err)
		}
		if verify.Status != "ok" {
			t.Fatalf("verify failed for %s: %+v", threadID, verify)
		}
	}
}

func TestRestoreIntoSameHomeCreatesForkWithoutTouchingSource(t *testing.T) {
	home := createFakeCodexHome(t)
	sourcePath := filepath.Join(home, "sessions", "2026", "06", "11", "rollout-2026-06-11T00-00-00-"+testThreadID+".jsonl")
	before, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: testThreadID, Out: out}); err != nil {
		t.Fatal(err)
	}
	targetCWD := t.TempDir()
	result, err := Restore(out, codex.RestoreOptions{Home: home, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ThreadID == testThreadID {
		t.Fatal("same-home import reused source thread id")
	}
	after, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("same-home import modified the source session file")
	}
	verify, err := Verify(home, result.ThreadID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("imported fork did not verify: %+v", verify)
	}
}

func TestExportAndRestoreLocalImageAssets(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	imagePath := filepath.Join(t.TempDir(), "upload.png")
	imageBytes := tinyPNG(t)
	if err := os.WriteFile(imagePath, imageBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes)
	sessionPath := fakeSessionPath(sourceHome)
	appendJSONL(t, sessionPath, map[string]any{
		"timestamp": "2026-06-11T00:00:04Z",
		"type":      "event_msg",
		"payload": map[string]any{
			"type":         "user_message",
			"local_images": []string{imagePath},
		},
	})
	appendJSONL(t, sessionPath, map[string]any{
		"timestamp": "2026-06-11T00:00:05Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type": "message",
			"role": "user",
			"content": []map[string]any{
				{"type": "input_text", "text": "please inspect\n<image name=\"upload.png\" path=\"" + imagePath + "\">\n# Files mentioned by the user\n- " + imagePath},
				{"type": "input_image", "image_url": dataURL, "detail": "high"},
			},
		},
	})

	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	exported, err := Export(ExportOptions{Home: sourceHome, Thread: testThreadID, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	if exported.Images.Copied != 1 || exported.Images.Embedded != 1 || exported.Images.Bytes == 0 {
		t.Fatalf("unexpected image summary: %+v", exported.Images)
	}
	var imageManifest ImageAssetsManifest
	if err := json.Unmarshal([]byte(readZipFile(t, out, ImageAssetsManifestPath)), &imageManifest); err != nil {
		t.Fatal(err)
	}
	if imageManifest.Schema != ImageAssetsSchema || len(imageManifest.Images) != 1 {
		t.Fatalf("unexpected image manifest: %+v", imageManifest)
	}
	asset := imageManifest.Images[0]
	if asset.Status != "copied" || asset.SourcePath != imagePath || asset.ZipPath == "" {
		t.Fatalf("unexpected image asset metadata: %+v", asset)
	}
	if got := readZipBytes(t, out, asset.ZipPath); string(got) != string(imageBytes) {
		t.Fatal("zip image payload mismatch")
	}
	inspected, err := Inspect(out)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Images.Copied != 1 || inspected.Images.Embedded != 1 {
		t.Fatalf("inspect image summary = %+v", inspected.Images)
	}

	targetHome := createEmptyCodexHome(t)
	targetCWD := t.TempDir()
	plan, err := Restore(out, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Images == nil || plan.Images.Copied != 1 {
		t.Fatalf("dry-run missing image plan: %+v", plan.Images)
	}
	if !containsPathSegment(plan.Writes, "agent-capsule-assets") {
		t.Fatalf("dry-run writes do not include image assets: %+v", plan.Writes)
	}
	result, err := Restore(out, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Images == nil || result.Images.Copied != 1 || result.Images.TargetDir == "" {
		t.Fatalf("execute missing image summary: %+v", result.Images)
	}
	restoredImagePath := filepath.Join(result.Images.TargetDir, filepath.Base(asset.ZipPath))
	if got, err := os.ReadFile(restoredImagePath); err != nil || string(got) != string(imageBytes) {
		t.Fatalf("restored image mismatch: len=%d err=%v", len(got), err)
	}
	restoredSession, err := os.ReadFile(result.TargetSessionPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(restoredSession), imagePath) {
		t.Fatal("restored session still contains source image path")
	}
	if !strings.Contains(string(restoredSession), restoredImagePath) {
		t.Fatal("restored session does not contain target image path")
	}
	if !strings.Contains(string(restoredSession), dataURL) {
		t.Fatal("input_image data URL was not preserved")
	}
}

func TestSecretScanBlocksExport(t *testing.T) {
	home := createFakeCodexHome(t)
	sessionPath := filepath.Join(home, "sessions", "2026", "06", "11", "rollout-2026-06-11T00-00-00-"+testThreadID+".jsonl")
	file, err := os.OpenFile(sessionPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString(`{"timestamp":"2026-06-11T00:00:04Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"sk-1234567890abcdefghijklmnopqrstuv"}]}}` + "\n")
	_ = file.Close()
	_, err = Export(ExportOptions{Home: home, Thread: testThreadID, Out: filepath.Join(t.TempDir(), "bad.capsule.zip")})
	if err == nil {
		t.Fatal("expected secret scan failure")
	}
	if _, err := Export(ExportOptions{Home: home, Thread: testThreadID, Out: filepath.Join(t.TempDir(), "allowed.capsule.zip"), UnsafeIncludeSecrets: true}); err != nil {
		t.Fatalf("unsafe export should be allowed: %v", err)
	}
}

func TestPreviewTranscriptIncludesMessagesAndToolSummaries(t *testing.T) {
	manifest := Manifest{
		ThreadID:    testThreadID,
		ThreadTitle: "Preview demo",
		SourceCWD:   "/source/project",
		CreatedAt:   "2026-06-12T00:00:00Z",
	}
	session := strings.Join([]string{
		`{"timestamp":"2026-06-12T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"please inspect"}]}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_1","arguments":"{\"cmd\":\"rg TODO\"}","status":"completed"}}`,
		`{"timestamp":"2026-06-12T00:00:03Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_1","output":"line 1\nline 2\n"}}`,
		`{"timestamp":"2026-06-12T00:00:04Z","type":"response_item","payload":{"type":"custom_tool_call","name":"apply_patch","call_id":"call_2","input":"*** Begin Patch\n..."}}`,
		`{"timestamp":"2026-06-12T00:00:05Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call_2","output":"Success"}}`,
		`{"timestamp":"2026-06-12T00:00:06Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
	}, "\n") + "\n"
	transcript := buildPreviewTranscript(manifest, []byte(session))
	if transcript.MessageCount != 2 {
		t.Fatalf("message_count = %d", transcript.MessageCount)
	}
	if transcript.ToolCount != 2 {
		t.Fatalf("tool_count = %d", transcript.ToolCount)
	}
	if len(transcript.Entries) != 4 {
		t.Fatalf("entries = %d", len(transcript.Entries))
	}
	if transcript.Entries[1].Tool != "functions.exec_command" {
		t.Fatalf("tool name = %q", transcript.Entries[1].Tool)
	}
	if transcript.Entries[1].OutputBytes == 0 {
		t.Fatal("missing output byte summary")
	}
	if transcript.Entries[1].Output != "line 1\nline 2\n" {
		t.Fatalf("tool output = %q", transcript.Entries[1].Output)
	}
	if strings.Contains(transcript.Entries[1].InputPreview, "line 1") {
		t.Fatal("tool output leaked into input preview instead of output field")
	}
}

func TestPreviewTranscriptSkipsRolledBackTurns(t *testing.T) {
	manifest := Manifest{
		ThreadID:    testThreadID,
		ThreadTitle: "Rollback demo",
		SourceCWD:   "/source/project",
		CreatedAt:   "2026-06-12T00:00:00Z",
	}
	transcript := buildPreviewTranscript(manifest, []byte(rolledBackSession(testThreadID)))
	if transcript.MessageCount != 3 {
		t.Fatalf("message_count = %d", transcript.MessageCount)
	}
	if transcript.ToolCount != 1 {
		t.Fatalf("tool_count = %d", transcript.ToolCount)
	}
	preview := previewEntriesText(transcript)
	if strings.Contains(preview, "rolled back") || strings.Contains(preview, "call_drop") {
		t.Fatalf("rolled-back turn leaked into preview:\n%s", preview)
	}
	for _, want := range []string{"keep first request", "keep current request", "functions.exec_command"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, preview)
		}
	}
}

func TestPreviewTranscriptSkipsOpenTurnRolledBackWithPreviousTurn(t *testing.T) {
	manifest := Manifest{
		ThreadID:    testThreadID,
		ThreadTitle: "Open rollback demo",
		SourceCWD:   "/source/project",
		CreatedAt:   "2026-06-12T00:00:00Z",
	}
	transcript := buildPreviewTranscript(manifest, []byte(openTurnRolledBackSession(testThreadID)))
	preview := previewEntriesText(transcript)
	for _, dropped := range []string{"drop previous request", "open aborted request"} {
		if strings.Contains(preview, dropped) {
			t.Fatalf("rolled-back open/current history leaked %q into preview:\n%s", dropped, preview)
		}
	}
	for _, want := range []string{"keep first request", "keep final request"} {
		if !strings.Contains(preview, want) {
			t.Fatalf("preview missing %q:\n%s", want, preview)
		}
	}
}

func TestPreviewTranscriptHidesInternalContextMessages(t *testing.T) {
	manifest := Manifest{
		ThreadID:    testThreadID,
		ThreadTitle: "Preview demo",
		CreatedAt:   "2026-06-12T00:00:00Z",
	}
	session := strings.Join([]string{
		`{"timestamp":"2026-06-12T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions for /repo\n\n<INSTRUCTIONS>\nsecret project instructions"}]}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"please inspect **this**"}]}}`,
	}, "\n") + "\n"
	transcript := buildPreviewTranscript(manifest, []byte(session))
	if transcript.MessageCount != 1 {
		t.Fatalf("message_count = %d", transcript.MessageCount)
	}
	if len(transcript.Entries) != 1 {
		t.Fatalf("entries = %d", len(transcript.Entries))
	}
	if strings.Contains(transcript.Entries[0].Text, "AGENTS.md") {
		t.Fatal("internal AGENTS.md context leaked into preview")
	}
}

func TestPreviewTranscriptAttachesSkillMessages(t *testing.T) {
	manifest := Manifest{
		ThreadID:    testThreadID,
		ThreadTitle: "Preview demo",
		CreatedAt:   "2026-06-12T00:00:00Z",
	}
	session := strings.Join([]string{
		`{"timestamp":"2026-06-12T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[$agent-capsule](/Users/me/.codex/skills/agent-capsule/SKILL.md) 导出一下这个 session"}]}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<skill>\n<name>agent-capsule</name>\n<path>/Users/me/.codex/skills/agent-capsule/SKILL.md</path>\n---\nname: agent-capsule\ndescription: Use when Codex needs to install or use Agent Capsule.\n</skill>"}]}}`,
		`{"timestamp":"2026-06-12T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"exported"}]}}`,
	}, "\n") + "\n"
	transcript := buildPreviewTranscript(manifest, []byte(session))
	if transcript.MessageCount != 2 {
		t.Fatalf("message_count = %d", transcript.MessageCount)
	}
	if len(transcript.Entries) != 2 {
		t.Fatalf("entries = %d", len(transcript.Entries))
	}
	if len(transcript.Entries[0].Skills) != 1 {
		t.Fatalf("skills = %+v", transcript.Entries[0].Skills)
	}
	skill := transcript.Entries[0].Skills[0]
	if skill.Name != "agent-capsule" || skill.Path != "/Users/me/.codex/skills/agent-capsule/SKILL.md" {
		t.Fatalf("skill metadata = %+v", skill)
	}
	if !strings.Contains(skill.Text, "Use when Codex needs") {
		t.Fatalf("skill text missing body: %+v", skill)
	}
	if strings.Contains(previewEntriesText(transcript), "<skill>") {
		t.Fatalf("skill body leaked as normal preview text:\n%s", previewEntriesText(transcript))
	}
}

func TestExportWritesOnlyActiveSessionBranch(t *testing.T) {
	home := createFakeCodexHomeWithSession(t, rolledBackSession(testThreadID))
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: testThreadID, Out: out}); err != nil {
		t.Fatal(err)
	}
	session := readZipFile(t, out, "codex/session.jsonl")
	if strings.Contains(session, "rolled back") || strings.Contains(session, "call_drop") {
		t.Fatalf("rolled-back turn leaked into export:\n%s", session)
	}
	if count := strings.Count(session, `"type":"session_meta"`); count != 1 {
		t.Fatalf("session_meta count = %d\n%s", count, session)
	}
	if !strings.Contains(session, "keep current request") {
		t.Fatalf("export missing current turn:\n%s", session)
	}
}

func TestExportKeepsCurrentTurnWithoutSelfExport(t *testing.T) {
	clearCurrentThreadEnv(t)
	home := createFakeCodexHomeWithSession(t, rolledBackSession(testThreadID))
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: "current", Out: out}); err != nil {
		t.Fatal(err)
	}
	session := readZipFile(t, out, "codex/session.jsonl")
	if !strings.Contains(session, "keep current request") {
		t.Fatalf("export missing non-export current turn:\n%s", session)
	}
}

func TestExportDropsOpenSelfExportTurn(t *testing.T) {
	clearCurrentThreadEnv(t)
	home := createFakeCodexHomeWithSession(t, selfExportSession(testThreadID, false))
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: "current", Out: out}); err != nil {
		t.Fatal(err)
	}
	session := readZipFile(t, out, "codex/session.jsonl")
	if !strings.Contains(session, "keep first request") {
		t.Fatalf("export dropped prior conversation:\n%s", session)
	}
	for _, dropped := range []string{"导出一下这个 session", "capsule export --thread current", "self export ready"} {
		if strings.Contains(session, dropped) {
			t.Fatalf("self-export turn leaked %q:\n%s", dropped, session)
		}
	}
}

func TestExportDropsCompletedSelfExportTurn(t *testing.T) {
	clearCurrentThreadEnv(t)
	home := createFakeCodexHomeWithSession(t, selfExportSession(testThreadID, true))
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: "current", Out: out}); err != nil {
		t.Fatal(err)
	}
	session := readZipFile(t, out, "codex/session.jsonl")
	if strings.Contains(session, "导出一下这个 session") || strings.Contains(session, "capsule export --thread current") {
		t.Fatalf("completed self-export turn leaked:\n%s", session)
	}
}

func TestExportKeepsMixedWorkAndSelfExportTurn(t *testing.T) {
	clearCurrentThreadEnv(t)
	home := createFakeCodexHomeWithSession(t, mixedWorkAndSelfExportSession(testThreadID))
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: "current", Out: out}); err != nil {
		t.Fatal(err)
	}
	session := readZipFile(t, out, "codex/session.jsonl")
	for _, want := range []string{"先改代码再导出这个 session", "apply_patch", "capsule export --thread current"} {
		if !strings.Contains(session, want) {
			t.Fatalf("mixed turn missing %q:\n%s", want, session)
		}
	}
}

func TestExportKeepsExplicitOtherThreadExportTurn(t *testing.T) {
	clearCurrentThreadEnv(t)
	home := createFakeCodexHomeWithSession(t, otherThreadExportSession(testThreadID))
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: "current", Out: out}); err != nil {
		t.Fatal(err)
	}
	session := readZipFile(t, out, "codex/session.jsonl")
	if !strings.Contains(session, "capsule export --thread 019e0000-0000-7000-8000-000000000123") {
		t.Fatalf("other-thread export turn was dropped:\n%s", session)
	}
}

func TestExportKeepsSearchMentioningSelfExportCommand(t *testing.T) {
	clearCurrentThreadEnv(t)
	home := createFakeCodexHomeWithSession(t, searchMentioningSelfExportSession(testThreadID))
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: "current", Out: out}); err != nil {
		t.Fatal(err)
	}
	session := readZipFile(t, out, "codex/session.jsonl")
	if !strings.Contains(session, "turn_search") || !strings.Contains(session, "call_search") {
		t.Fatalf("search-only turn was dropped:\n%s", session)
	}
}

func clearCurrentThreadEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CODEX_THREAD_ID", "")
	t.Setenv("CODEX_SESSION_ID", "")
}

func TestExportDropsOpenTurnRolledBackWithPreviousTurn(t *testing.T) {
	home := createFakeCodexHomeWithSession(t, openTurnRolledBackSession(testThreadID))
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: home, Thread: testThreadID, Out: out}); err != nil {
		t.Fatal(err)
	}
	session := readZipFile(t, out, "codex/session.jsonl")
	for _, dropped := range []string{"drop previous request", "open aborted request"} {
		if strings.Contains(session, dropped) {
			t.Fatalf("rolled-back open/current history leaked %q into export:\n%s", dropped, session)
		}
	}
	for _, want := range []string{"keep first request", "keep final request"} {
		if !strings.Contains(session, want) {
			t.Fatalf("export missing %q:\n%s", want, session)
		}
	}
	if count := strings.Count(session, `"type":"session_meta"`); count != 1 {
		t.Fatalf("session_meta count = %d\n%s", count, session)
	}
}

func TestRestoreNormalizesLegacyRolledBackCapsule(t *testing.T) {
	out := writeLegacyRolledBackCapsule(t)
	targetHome := createEmptyCodexHome(t)
	targetCWD := t.TempDir()
	result, err := Restore(out, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(result.TargetSessionPath)
	if err != nil {
		t.Fatal(err)
	}
	session := string(content)
	if strings.Contains(session, "rolled back") || strings.Contains(session, "call_drop") {
		t.Fatalf("rolled-back turn leaked into restore:\n%s", session)
	}
	if count := strings.Count(session, `"type":"session_meta"`); count != 1 {
		t.Fatalf("session_meta count = %d\n%s", count, session)
	}
	if !strings.Contains(session, result.ThreadID) {
		t.Fatalf("restored session missing target thread id %q:\n%s", result.ThreadID, session)
	}
	verify, err := Verify(targetHome, result.ThreadID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("verify failed: %+v", verify)
	}
}

func TestPreviewTranscriptIncludesPureImageMessages(t *testing.T) {
	manifest := Manifest{
		ThreadID:    testThreadID,
		ThreadTitle: "Preview demo",
		CreatedAt:   "2026-06-12T00:00:00Z",
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(tinyPNG(t))
	session := `{"timestamp":"2026-06-12T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_image","image_url":"` + dataURL + `","detail":"high"}]}}` + "\n"
	transcript := buildPreviewTranscript(manifest, []byte(session))
	if transcript.MessageCount != 1 || len(transcript.Entries) != 1 {
		t.Fatalf("unexpected transcript: %+v", transcript)
	}
	if transcript.Entries[0].Text != "" || len(transcript.Entries[0].Images) != 1 {
		t.Fatalf("pure image message was not preserved: %+v", transcript.Entries[0])
	}
	if transcript.Entries[0].Images[0].Src != dataURL {
		t.Fatal("preview image src mismatch")
	}
}

func TestPreviewTranscriptPrefersEmbeddedImageOverLocalFallback(t *testing.T) {
	manifest := Manifest{
		ThreadID:    testThreadID,
		ThreadTitle: "Preview demo",
		CreatedAt:   "2026-06-12T00:00:00Z",
	}
	imageBytes := tinyPNG(t)
	localPath := "/tmp/codex-clipboard-upload.png"
	localAsset := imageAssetFile{
		Metadata: ImageAssetMetadata{
			SourcePath:   localPath,
			MIME:         "image/png",
			OriginalName: "upload.png",
			Status:       "copied",
		},
		Content: imageBytes,
	}
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("embedded"))
	session := `{"timestamp":"2026-06-12T00:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<image name=\"upload.png\" path=\"` + localPath + `\">"},{"type":"input_image","image_url":"` + dataURL + `","detail":"high"}]}}` + "\n"
	transcript := buildPreviewTranscript(manifest, []byte(session), localAsset)
	if len(transcript.Entries) != 1 || len(transcript.Entries[0].Images) != 1 {
		t.Fatalf("unexpected image entries: %+v", transcript.Entries)
	}
	if transcript.Entries[0].Images[0].Src != dataURL {
		t.Fatal("preview did not prefer embedded input_image data URL")
	}
}

func TestPreviewTranscriptOmitsImagesAfterSoftLimit(t *testing.T) {
	manifest := Manifest{
		ThreadID:    testThreadID,
		ThreadTitle: "Preview demo",
		CreatedAt:   "2026-06-12T00:00:00Z",
	}
	var content []map[string]any
	for i := 0; i < maxPreviewImages+1; i++ {
		content = append(content, map[string]any{
			"type":      "input_image",
			"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte{byte(i)}),
		})
	}
	line := map[string]any{
		"timestamp": "2026-06-12T00:00:01Z",
		"type":      "response_item",
		"payload": map[string]any{
			"type":    "message",
			"role":    "user",
			"content": content,
		},
	}
	payload, err := json.Marshal(line)
	if err != nil {
		t.Fatal(err)
	}
	transcript := buildPreviewTranscript(manifest, append(payload, '\n'))
	if len(transcript.Entries) != 1 {
		t.Fatalf("entries = %d", len(transcript.Entries))
	}
	entry := transcript.Entries[0]
	if len(entry.Images) != maxPreviewImages || entry.OmittedImages != 1 {
		t.Fatalf("soft limit not enforced: images=%d omitted=%d", len(entry.Images), entry.OmittedImages)
	}
}

func TestShareWorkerManifestIncludesPreviewAndAgentCommands(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	var captured LinkManifest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(WorkerCapabilities{Schema: LinkSchema, Service: "test", QuotaPolicy: "anonymous-small"})
		case "/v1/shares":
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			if err := json.Unmarshal([]byte(r.FormValue("manifest")), &captured); err != nil {
				t.Fatalf("decode manifest: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(workerShareResponse{
				ShareURL:    serverURL(r) + "/s/test-share",
				ManifestURL: serverURL(r) + "/v1/shares/test-share",
				ExpiresAt:   "2026-06-13T00:00:00Z",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result, err := Share(ShareOptions{Home: sourceHome, Thread: testThreadID, Service: "worker", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" {
		t.Fatalf("status = %q", result.Status)
	}
	if captured.Import.InstallCommand != InstallCmd {
		t.Fatalf("install command = %q", captured.Import.InstallCommand)
	}
	if captured.Import.ExecuteCommand == "" || captured.Import.DocsURL != DefaultRepo || captured.Import.SkillURL != DefaultSkill {
		t.Fatalf("missing import metadata: %+v", captured.Import)
	}
	if captured.Preview == nil {
		t.Fatal("missing encrypted preview")
	}
	if captured.Preview.Schema != PreviewSchema {
		t.Fatalf("preview schema = %q", captured.Preview.Schema)
	}
	key := linkKey(t, result.ShareURL)
	transcript := decryptPreview(t, captured.Preview, key)
	if transcript.ThreadID != testThreadID {
		t.Fatalf("preview thread id = %q", transcript.ThreadID)
	}
	if transcript.MessageCount == 0 {
		t.Fatal("preview did not include messages")
	}
}

func TestOfficialEndpointResolution(t *testing.T) {
	t.Setenv("CAPSULE_OFFICIAL_ENDPOINT", "")
	if got := resolveWorkerEndpoint("official", ""); got != DefaultOfficialEndpoint {
		t.Fatalf("default official endpoint = %q", got)
	}

	t.Setenv("CAPSULE_OFFICIAL_ENDPOINT", "https://override.example/")
	if got := resolveWorkerEndpoint("official", ""); got != "https://override.example" {
		t.Fatalf("env official endpoint = %q", got)
	}

	if got := resolveWorkerEndpoint("official", "https://explicit.example/"); got != "https://explicit.example" {
		t.Fatalf("explicit official endpoint = %q", got)
	}
}

func TestRestorePreservesNewSQLiteColumns(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	if _, err := Export(ExportOptions{Home: sourceHome, Thread: testThreadID, Out: out}); err != nil {
		t.Fatal(err)
	}
	targetHome := createEmptyCodexHome(t)
	targetCWD := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(targetHome, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var model, effort, preview string
	result, err := Restore(out, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("select model, reasoning_effort, preview from threads where id = ?", result.ThreadID).Scan(&model, &effort, &preview); err != nil {
		t.Fatal(err)
	}
	if model != "gpt-5.5" || effort != "xhigh" || preview == "" {
		t.Fatalf("new columns not preserved: model=%q effort=%q preview=%q", model, effort, preview)
	}
}

func TestRestoreFromURLDownloadsDecryptsAndImportsAsNewThread(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	out := filepath.Join(t.TempDir(), "session.capsule.zip")
	exported, err := Export(ExportOptions{Home: sourceHome, Thread: testThreadID, Out: out})
	if err != nil {
		t.Fatal(err)
	}
	plain, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := encryptCapsule(plain)
	if err != nil {
		t.Fatal(err)
	}
	var manifest LinkManifest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(manifest)
		case "/blob":
			_, _ = w.Write(enc.Ciphertext)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	manifest = buildLinkManifest(exported, enc, "worker")
	manifest.Bundle.URL = server.URL + "/blob"
	link := appendKeyFragment(server.URL+"/manifest", enc.Key)
	targetHome := createEmptyCodexHome(t)
	targetCWD := t.TempDir()
	result, err := RestoreFromURL(link, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ThreadID == testThreadID {
		t.Fatal("URL import reused source thread id")
	}
	verify, err := Verify(targetHome, result.ThreadID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("verify failed: %+v", verify)
	}
}

func TestRestoreFromURLRejectsBadLinksBeforeWriting(t *testing.T) {
	targetHome := createEmptyCodexHome(t)
	if _, err := RestoreFromURL("https://example.test/manifest", codex.RestoreOptions{Home: targetHome, TargetCWD: t.TempDir(), Execute: true}); err == nil || !strings.Contains(err.Error(), "missing #k=") {
		t.Fatalf("expected missing key error, got %v", err)
	}
}

func TestShareWorkerFailureFallsBackToZip(t *testing.T) {
	t.Chdir(t.TempDir())
	sourceHome := createFakeCodexHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(WorkerCapabilities{Schema: LinkSchema, Service: "test", QuotaPolicy: "anonymous-small"})
		case "/v1/shares":
			http.Error(w, "quota exceeded", http.StatusInsufficientStorage)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result, err := Share(ShareOptions{Home: sourceHome, Thread: testThreadID, Service: "worker", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "fallback_zip" {
		t.Fatalf("status = %q", result.Status)
	}
	if result.Path == "" {
		t.Fatal("missing fallback path")
	}
	if _, err := os.Stat(result.Path); err != nil {
		t.Fatal(err)
	}
}

func TestShareWorkerAllowsMissingCapabilitiesWithWarning(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/capabilities":
			http.NotFound(w, r)
		case "/v1/shares":
			if err := r.ParseMultipartForm(8 << 20); err != nil {
				t.Fatalf("parse multipart: %v", err)
			}
			if r.FormValue("manifest") == "" {
				t.Fatal("missing manifest field")
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(workerShareResponse{
				ShareURL:    serverURL(r) + "/s/test-share",
				ManifestURL: serverURL(r) + "/v1/shares/test-share",
				ExpiresAt:   "2026-06-13T00:00:00Z",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result, err := Share(ShareOptions{Home: sourceHome, Thread: testThreadID, Service: "worker", Endpoint: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.ShareURL == "" {
		t.Fatalf("unexpected share result: %+v", result)
	}
	if len(result.Warnings) == 0 || !strings.Contains(result.Warnings[0], "quota_policy=unknown") {
		t.Fatalf("missing capability warning: %+v", result.Warnings)
	}
	if !strings.Contains(result.ShareURL, "#k=") {
		t.Fatalf("share url missing key fragment: %s", result.ShareURL)
	}
}

func TestShareS3RoundTrip(t *testing.T) {
	sourceHome := createFakeCodexHome(t)
	targetHome := createEmptyCodexHome(t)
	targetCWD := t.TempDir()
	var mu sync.Mutex
	objects := map[string][]byte{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/")
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			objects[key] = body
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			mu.Lock()
			body, ok := objects[key]
			mu.Unlock()
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	result, err := Share(ShareOptions{
		Home:    sourceHome,
		Thread:  testThreadID,
		Service: "s3",
		S3: S3Options{
			Endpoint:        server.URL,
			Bucket:          "capsules",
			Prefix:          "test",
			AccessKeyID:     "test-key",
			SecretAccessKey: "test-secret",
			Region:          "auto",
			PublicBaseURL:   server.URL + "/capsules",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "ok" || result.ShareURL == "" {
		t.Fatalf("unexpected share result: %+v", result)
	}
	imported, err := RestoreFromURL(result.ShareURL, codex.RestoreOptions{Home: targetHome, TargetCWD: targetCWD, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	if imported.ThreadID == testThreadID {
		t.Fatal("S3 URL import reused source thread id")
	}
	verify, err := Verify(targetHome, imported.ThreadID, targetCWD)
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("verify failed: %+v", verify)
	}
}

func createFakeCodexHome(t *testing.T) string {
	t.Helper()
	home := createEmptyCodexHome(t)
	sessionDir := filepath.Join(home, "sessions", "2026", "06", "11")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(sessionDir, "rollout-2026-06-11T00-00-00-"+testThreadID+".jsonl")
	lines := []string{
		`{"timestamp":"2026-06-11T00:00:00Z","type":"session_meta","payload":{"id":"` + testThreadID + `","timestamp":"2026-06-11T00:00:00Z","cwd":"/source/project","cli_version":"0.138.0-alpha.7","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-11T00:00:01Z","type":"turn_context","payload":{"cwd":"/source/project","approval_policy":"never"}}`,
		`{"timestamp":"2026-06-11T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"share this session"}]}}`,
		`{"timestamp":"2026-06-11T00:00:03Z","type":"event_msg","payload":{"type":"agent_message","message":"ready to restore"}}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(home, "session_index.jsonl"), map[string]any{
		"id":          testThreadID,
		"thread_name": "Test Session",
		"updated_at":  "2026-06-11T00:00:03Z",
	})
	db := openTestDB(t, home)
	defer db.Close()
	insertThreadRow(t, db, sessionPath)
	return home
}

func createFakeClaudeHome(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "claude-source")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(home, "projects", claude.ProjectDirName(cwd))
	if err := os.MkdirAll(projectDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionPath := filepath.Join(projectDir, testClaudeSessionID+".jsonl")
	lines := []string{
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-06-11T00:00:00Z","sessionId":"` + testClaudeSessionID + `"}`,
		`{"type":"queue-operation","operation":"dequeue","timestamp":"2026-06-11T00:00:00Z","sessionId":"` + testClaudeSessionID + `"}`,
		`{"parentUuid":null,"isSidechain":false,"type":"user","message":{"role":"user","content":"Claude asks for help"},"uuid":"5b00343b-dfbe-45cd-b6f6-60c04f157721","timestamp":"2026-06-11T00:00:01Z","userType":"external","entrypoint":"claude-code","cwd":"` + cwd + `","sessionId":"` + testClaudeSessionID + `","version":"2.0.45","gitBranch":"main"}`,
		`{"parentUuid":"5b00343b-dfbe-45cd-b6f6-60c04f157721","isSidechain":false,"type":"assistant","uuid":"00c879d2-fe66-4e95-a1f1-5d9b1d4cc6a5","timestamp":"2026-06-11T00:00:02Z","message":{"role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"Claude answer"}],"usage":{}},"userType":"external","entrypoint":"claude-code","cwd":"` + cwd + `","sessionId":"` + testClaudeSessionID + `","version":"2.0.45","gitBranch":"main"}`,
		`{"type":"last-prompt","lastPrompt":"Claude asks for help","leafUuid":"00c879d2-fe66-4e95-a1f1-5d9b1d4cc6a5","sessionId":"` + testClaudeSessionID + `"}`,
	}
	if err := os.WriteFile(sessionPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	index := map[string]any{
		"version":      1,
		"originalPath": cwd,
		"entries": []map[string]any{{
			"sessionId":    testClaudeSessionID,
			"fullPath":     sessionPath,
			"fileMtime":    int64(1781136002000),
			"firstPrompt":  "Claude asks for help",
			"messageCount": 2,
			"created":      "2026-06-11T00:00:01Z",
			"modified":     "2026-06-11T00:00:02Z",
			"gitBranch":    "main",
			"projectPath":  cwd,
			"isSidechain":  false,
		}},
	}
	payload, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "sessions-index.json"), append(payload, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return home, cwd
}

func createFakeCodexHomeWithSession(t *testing.T, session string) string {
	t.Helper()
	home := createEmptyCodexHome(t)
	sessionPath := fakeSessionPath(home)
	if err := os.MkdirAll(filepath.Dir(sessionPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionPath, []byte(session), 0o644); err != nil {
		t.Fatal(err)
	}
	writeJSONL(t, filepath.Join(home, "session_index.jsonl"), map[string]any{
		"id":          testThreadID,
		"thread_name": "Rollback demo",
		"updated_at":  "2026-06-12T00:00:10Z",
	})
	db := openTestDB(t, home)
	defer db.Close()
	insertThreadRow(t, db, sessionPath)
	return home
}

func rolledBackSession(threadID string) string {
	ancestorID := "019e0000-0000-7000-8000-000000000099"
	lines := []string{
		`{"timestamp":"2026-06-12T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","forked_from_id":"` + ancestorID + `","timestamp":"2026-06-12T00:00:00Z","cwd":"/source/project","cli_version":"0.140.0-alpha.2","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-12T00:00:00Z","type":"session_meta","payload":{"id":"` + ancestorID + `","timestamp":"2026-06-11T00:00:00Z","cwd":"/source/project","cli_version":"0.140.0-alpha.2","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-12T00:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_keep_first"}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep first request"}]}}`,
		`{"timestamp":"2026-06-12T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"keep first answer"}]}}`,
		`{"timestamp":"2026-06-12T00:00:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_keep_first","duration_ms":1}}`,
		`{"timestamp":"2026-06-12T00:00:05Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_drop"}}`,
		`{"timestamp":"2026-06-12T00:00:06Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"rolled back request"}]}}`,
		`{"timestamp":"2026-06-12T00:00:07Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_drop","arguments":"{\"cmd\":\"false\"}"}}`,
		`{"timestamp":"2026-06-12T00:00:08Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_drop","output":"rolled back output"}}`,
		`{"timestamp":"2026-06-12T00:00:09Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"rolled back answer"}]}}`,
		`{"timestamp":"2026-06-12T00:00:10Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_drop","duration_ms":1}}`,
		`{"timestamp":"2026-06-12T00:00:11Z","type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":1}}`,
		`{"timestamp":"2026-06-12T00:00:12Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_keep_current"}}`,
		`{"timestamp":"2026-06-12T00:00:13Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep current request"}]}}`,
		`{"timestamp":"2026-06-12T00:00:14Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_keep","arguments":"{\"cmd\":\"true\"}"}}`,
		`{"timestamp":"2026-06-12T00:00:15Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_keep","output":"keep current output"}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func openTurnRolledBackSession(threadID string) string {
	ancestorID := "019e0000-0000-7000-8000-000000000099"
	lines := []string{
		`{"timestamp":"2026-06-12T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","forked_from_id":"` + ancestorID + `","timestamp":"2026-06-12T00:00:00Z","cwd":"/source/project","cli_version":"0.140.0-alpha.2","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-12T00:00:00Z","type":"session_meta","payload":{"id":"` + ancestorID + `","timestamp":"2026-06-11T00:00:00Z","cwd":"/source/project","cli_version":"0.140.0-alpha.2","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-12T00:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_keep_first"}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep first request"}]}}`,
		`{"timestamp":"2026-06-12T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"keep first answer"}]}}`,
		`{"timestamp":"2026-06-12T00:00:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_keep_first","duration_ms":1}}`,
		`{"timestamp":"2026-06-12T00:00:05Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_drop_previous"}}`,
		`{"timestamp":"2026-06-12T00:00:06Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"drop previous request"}]}}`,
		`{"timestamp":"2026-06-12T00:00:07Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_drop_previous","duration_ms":1}}`,
		`{"timestamp":"2026-06-12T00:00:08Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_open_abort"}}`,
		`{"timestamp":"2026-06-12T00:00:09Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"open aborted request"}]}}`,
		`{"timestamp":"2026-06-12T00:00:10Z","type":"event_msg","payload":{"type":"thread_rolled_back","num_turns":2}}`,
		`{"timestamp":"2026-06-12T00:00:11Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_keep_final"}}`,
		`{"timestamp":"2026-06-12T00:00:12Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep final request"}]}}`,
		`{"timestamp":"2026-06-12T00:00:13Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"keep final answer"}]}}`,
		`{"timestamp":"2026-06-12T00:00:14Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_keep_final","duration_ms":1}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func selfExportSession(threadID string, complete bool) string {
	lines := []string{
		`{"timestamp":"2026-06-12T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","timestamp":"2026-06-12T00:00:00Z","cwd":"/source/project","cli_version":"0.140.0-alpha.2","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-12T00:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_keep_first"}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep first request"}]}}`,
		`{"timestamp":"2026-06-12T00:00:03Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"keep first answer"}]}}`,
		`{"timestamp":"2026-06-12T00:00:04Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_keep_first","duration_ms":1}}`,
		`{"timestamp":"2026-06-12T00:00:05Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_self_export"}}`,
		`{"timestamp":"2026-06-12T00:00:06Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"[$agent-capsule](/Users/me/.codex/skills/agent-capsule/SKILL.md) 导出一下这个 session"}]}}`,
		`{"timestamp":"2026-06-12T00:00:07Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<skill>\n<name>agent-capsule</name>\n<path>/Users/me/.codex/skills/agent-capsule/SKILL.md</path>\n</skill>"}]}}`,
		`{"timestamp":"2026-06-12T00:00:08Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"我会导出当前 session。"}]}}`,
		`{"timestamp":"2026-06-12T00:00:09Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_skill","arguments":"{\"cmd\":\"sed -n '1,120p' /Users/me/.codex/skills/agent-capsule/SKILL.md\"}"}}`,
		`{"timestamp":"2026-06-12T00:00:10Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_skill","output":"skill text"}}`,
		`{"timestamp":"2026-06-12T00:00:11Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_which","arguments":"{\"cmd\":\"command -v capsule\"}"}}`,
		`{"timestamp":"2026-06-12T00:00:12Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_which","output":"/Users/me/.local/bin/capsule"}}`,
		`{"timestamp":"2026-06-12T00:00:13Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_export","arguments":"{\"cmd\":\"capsule export --thread current\"}"}}`,
		`{"timestamp":"2026-06-12T00:00:14Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_export","output":"self export ready"}}`,
		`{"timestamp":"2026-06-12T00:00:15Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"导出了。"}]}}`,
	}
	if complete {
		lines = append(lines, `{"timestamp":"2026-06-12T00:00:16Z","type":"event_msg","payload":{"type":"task_complete","turn_id":"turn_self_export","duration_ms":1}}`)
	}
	return strings.Join(lines, "\n") + "\n"
}

func mixedWorkAndSelfExportSession(threadID string) string {
	lines := []string{
		`{"timestamp":"2026-06-12T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","timestamp":"2026-06-12T00:00:00Z","cwd":"/source/project","cli_version":"0.140.0-alpha.2","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-12T00:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_mixed"}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"先改代码再导出这个 session"}]}}`,
		`{"timestamp":"2026-06-12T00:00:03Z","type":"response_item","payload":{"type":"function_call","name":"apply_patch","call_id":"call_patch","arguments":"*** Begin Patch\n*** End Patch\n"}}`,
		`{"timestamp":"2026-06-12T00:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_patch","output":"Success"}}`,
		`{"timestamp":"2026-06-12T00:00:05Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_export","arguments":"{\"cmd\":\"capsule export --thread current\"}"}}`,
		`{"timestamp":"2026-06-12T00:00:06Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_export","output":"exported"}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func otherThreadExportSession(threadID string) string {
	otherThreadID := "019e0000-0000-7000-8000-000000000123"
	lines := []string{
		`{"timestamp":"2026-06-12T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","timestamp":"2026-06-12T00:00:00Z","cwd":"/source/project","cli_version":"0.140.0-alpha.2","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-12T00:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_other_export"}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"导出另一个 thread"}]}}`,
		`{"timestamp":"2026-06-12T00:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_export","arguments":"{\"cmd\":\"capsule export --thread ` + otherThreadID + `\"}"}}`,
		`{"timestamp":"2026-06-12T00:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_export","output":"exported"}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func searchMentioningSelfExportSession(threadID string) string {
	lines := []string{
		`{"timestamp":"2026-06-12T00:00:00Z","type":"session_meta","payload":{"id":"` + threadID + `","timestamp":"2026-06-12T00:00:00Z","cwd":"/source/project","cli_version":"0.140.0-alpha.2","source":"vscode","thread_source":"user","model_provider":"openai"}}`,
		`{"timestamp":"2026-06-12T00:00:01Z","type":"event_msg","payload":{"type":"task_started","turn_id":"turn_search"}}`,
		`{"timestamp":"2026-06-12T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"查一下怎么导出这个 session"}]}}`,
		`{"timestamp":"2026-06-12T00:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","namespace":"functions","call_id":"call_search","arguments":"{\"cmd\":\"rg \\\"capsule export --thread current\\\" README.md\"}"}}`,
		`{"timestamp":"2026-06-12T00:00:04Z","type":"response_item","payload":{"type":"function_call_output","call_id":"call_search","output":"README.md:capsule export --thread current"}}`,
	}
	return strings.Join(lines, "\n") + "\n"
}

func writeLegacyRolledBackCapsule(t *testing.T) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "legacy.capsule.zip")
	manifest := Manifest{
		SchemaVersion:             SchemaVersion,
		ArtifactType:              ArtifactType,
		CreatedAt:                 "2026-06-12T00:00:00Z",
		ThreadID:                  testThreadID,
		ThreadTitle:               "Rollback demo",
		SourceHome:                "/source/home",
		SourceCWD:                 "/source/project",
		SourceSessionRelativePath: "sessions/2026/06/12/rollout-" + testThreadID + ".jsonl",
		RepoURL:                   DefaultRepo,
		SkillURL:                  DefaultSkill,
		InstallCommand:            InstallCmd,
		RestoreCommand:            "capsule import <this-file>.capsule.zip --target codex --target-cwd . --execute",
		Files:                     append([]string(nil), RequiredFiles...),
	}
	manifestPayload, err := jsonBytes(manifest)
	if err != nil {
		t.Fatal(err)
	}
	indexPayload, err := jsonBytes(map[string]any{
		"id":          testThreadID,
		"thread_name": "Rollback demo",
		"updated_at":  "2026-06-12T00:00:10Z",
	})
	if err != nil {
		t.Fatal(err)
	}
	threadPayload, err := jsonBytes(map[string]any{
		"id":           testThreadID,
		"title":        "Rollback demo",
		"cwd":          "/source/project",
		"rollout_path": manifest.SourceSessionRelativePath,
	})
	if err != nil {
		t.Fatal(err)
	}
	scanPayload, err := jsonBytes(SafetyScan{Status: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"manifest.json":          manifestPayload,
		"AGENT_README.md":        []byte("legacy capsule"),
		"codex/session.jsonl":    []byte(rolledBackSession(testThreadID)),
		"codex/index-entry.json": indexPayload,
		"codex/thread-row.json":  threadPayload,
		"agent/restore.md":       []byte("legacy restore"),
		"safety/scan.json":       scanPayload,
	}
	checksums := buildChecksums(files)
	checksumPayload, err := jsonBytes(checksums)
	if err != nil {
		t.Fatal(err)
	}
	files["checksums.json"] = checksumPayload
	if err := writeZip(out, files); err != nil {
		t.Fatal(err)
	}
	return out
}

func fakeSessionPath(home string) string {
	return filepath.Join(home, "sessions", "2026", "06", "11", "rollout-2026-06-11T00-00-00-"+testThreadID+".jsonl")
}

func serverURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func createEmptyCodexHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	db := openTestDB(t, home)
	defer db.Close()
	return home
}

func openTestDB(t *testing.T, home string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	schema := `create table if not exists threads (
id text primary key,
rollout_path text not null,
created_at integer not null,
updated_at integer not null,
source text not null,
model_provider text not null,
cwd text not null,
title text not null,
sandbox_policy text not null,
approval_mode text not null,
tokens_used integer not null default 0,
has_user_event integer not null default 0,
archived integer not null default 0,
archived_at integer,
git_sha text,
git_branch text,
git_origin_url text,
cli_version text not null default '',
first_user_message text not null default '',
agent_nickname text,
agent_role text,
memory_mode text not null default 'enabled',
model text,
reasoning_effort text,
agent_path text,
created_at_ms integer,
updated_at_ms integer,
thread_source text,
preview text not null default ''
)`
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	return db
}

func insertThreadRow(t *testing.T, db *sql.DB, sessionPath string) {
	t.Helper()
	_, err := db.Exec(`insert into threads (
id, rollout_path, created_at, updated_at, source, model_provider, cwd, title,
sandbox_policy, approval_mode, tokens_used, has_user_event, archived,
git_sha, git_branch, git_origin_url, cli_version, first_user_message,
memory_mode, model, reasoning_effort, created_at_ms, updated_at_ms,
thread_source, preview
) values (?, ?, 1781136000, 1781136003, 'vscode', 'openai', '/source/project', 'Test Session',
'{"type":"disabled"}', 'never', 42, 1, 0,
'abc123', 'main', 'git@example.com:test/repo.git', '0.138.0-alpha.7', 'share this session',
'enabled', 'gpt-5.5', 'xhigh', 1781136000000, 1781136003000,
'user', 'Test Session preview')`, testThreadID, sessionPath)
	if err != nil {
		t.Fatal(err)
	}
}

func writeJSONL(t *testing.T, path string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(payload, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func appendJSONL(t *testing.T, path string, value any) {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.Write(append(payload, '\n')); err != nil {
		t.Fatal(err)
	}
}

func readIndexEntry(t *testing.T, home, threadID string) map[string]any {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(home, "session_index.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["id"] == threadID {
			return entry
		}
	}
	t.Fatalf("missing session index entry for %s", threadID)
	return nil
}

func verifySessionPath(t *testing.T, home, threadID string) string {
	t.Helper()
	row := readSQLiteThreadRowForTest(t, home, threadID)
	path, _ := row["rollout_path"].(string)
	if path == "" {
		t.Fatalf("missing rollout_path for %s", threadID)
	}
	return path
}

func readSQLiteThreadRowForTest(t *testing.T, home, threadID string) map[string]any {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(home, "state_5.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("select * from threads where id = ?", threadID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("missing sqlite row for %s", threadID)
	}
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	values := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	for i, column := range columns {
		switch value := values[i].(type) {
		case []byte:
			out[column] = string(value)
		default:
			out[column] = value
		}
	}
	return out
}

func readZipFile(t *testing.T, path, name string) string {
	t.Helper()
	return string(readZipBytes(t, path, name))
}

func readZipBytes(t *testing.T, path, name string) []byte {
	t.Helper()
	reader, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	for _, file := range reader.File {
		if file.Name != name {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		content, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return content
	}
	t.Fatalf("missing zip file %s", name)
	return nil
}

func previewEntriesText(transcript PreviewTranscript) string {
	var b strings.Builder
	for _, entry := range transcript.Entries {
		b.WriteString(entry.Kind)
		b.WriteString(" ")
		b.WriteString(entry.Role)
		b.WriteString(" ")
		b.WriteString(entry.Tool)
		b.WriteString(" ")
		b.WriteString(entry.Text)
		b.WriteString(" ")
		b.WriteString(entry.InputPreview)
		b.WriteString(" ")
		b.WriteString(entry.Output)
		b.WriteString("\n")
	}
	return b.String()
}

func containsPathSegment(paths []string, segment string) bool {
	for _, path := range paths {
		if strings.Contains(path, segment) {
			return true
		}
	}
	return false
}

func tinyPNG(t *testing.T) []byte {
	t.Helper()
	content, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/p9sAAAAASUVORK5CYII=")
	if err != nil {
		t.Fatal(err)
	}
	return content
}

func linkKey(t *testing.T, rawURL string) []byte {
	t.Helper()
	_, key, err := parseLinkKey(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func decryptPreview(t *testing.T, preview *LinkPreview, key []byte) PreviewTranscript {
	t.Helper()
	nonce, err := base64.RawURLEncoding.DecodeString(preview.Crypto.Nonce)
	if err != nil {
		t.Fatal(err)
	}
	ciphertext, err := base64.RawURLEncoding.DecodeString(preview.Payload)
	if err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		t.Fatal(err)
	}
	var transcript PreviewTranscript
	if err := json.Unmarshal(plain, &transcript); err != nil {
		t.Fatal(err)
	}
	return transcript
}
