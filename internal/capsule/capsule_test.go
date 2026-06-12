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

	"github.com/z2z23n0/agent-capsule/internal/codex"
)

const testThreadID = "019e0000-0000-7000-8000-000000000001"

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
	if strings.Contains(transcript.Entries[1].InputPreview, "line 1") {
		t.Fatal("tool output leaked into input preview")
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
	if captured.Import.DryRunCommand == "" || captured.Import.ExecuteCommand == "" || captured.Import.DocsURL != DefaultRepo {
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

func readZipFile(t *testing.T, path, name string) string {
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
		return string(content)
	}
	t.Fatalf("missing zip file %s", name)
	return ""
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
