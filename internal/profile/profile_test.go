package profile

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestProfileExportImportAndVerify(t *testing.T) {
	env := newMigrationTestEnv(t)
	result, err := Export(ExportOptions{
		Home:            env.sourceHome,
		TargetHome:      env.targetHome,
		TargetWorkspace: env.targetWorkspace,
		Projects:        []string{env.sourceProject},
		Out:             env.bundle,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Manifest.Stats.Threads != 1 {
		t.Fatalf("threads = %d, want 1", result.Manifest.Stats.Threads)
	}
	assertManifestExcludes(t, result.Manifest, "auth.json")
	assertManifestExcludes(t, result.Manifest, "skills/.system/managed.md")
	assertManifestIncludes(t, result.Manifest, "skills/user-skill/SKILL.md")
	assertManifestIncludes(t, result.Manifest, "skills/linked-skill")
	assertManifestIncludes(t, result.Manifest, "memories_1.sqlite")

	cloneRepo(t, env.sourceProject, env.targetProject)
	dryRun, err := Import(ImportOptions{BundleDir: env.bundle, Home: env.targetHome})
	if err != nil {
		t.Fatal(err)
	}
	if !dryRun.DryRun || dryRun.Status != "planned" {
		t.Fatalf("unexpected dry run: %#v", dryRun)
	}
	imported, err := Import(ImportOptions{BundleDir: env.bundle, Home: env.targetHome, Execute: true})
	if err != nil {
		t.Fatal(err)
	}
	if imported.Status != "ok" || imported.BackupDir == "" || !imported.PreservedIdentity {
		t.Fatalf("unexpected import: %#v", imported)
	}
	if got := readFile(t, filepath.Join(env.targetHome, "auth.json")); got != "target-auth" {
		t.Fatalf("target auth changed: %q", got)
	}
	if got := readFile(t, filepath.Join(env.targetHome, "skills", ".system", "managed.md")); got != "target-managed" {
		t.Fatalf("managed skill changed: %q", got)
	}
	if _, err := os.Stat(filepath.Join(env.targetHome, "skills", "stale", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("stale user skill was not pruned: %v", err)
	}
	if got := readFile(t, filepath.Join(imported.BackupDir, "skills", "stale", "SKILL.md")); got != "stale" {
		t.Fatalf("stale skill was not backed up: %q", got)
	}
	if target, err := os.Readlink(filepath.Join(imported.BackupDir, "skills", "linked-skill")); err != nil || target != "/stale/linked-skill" {
		t.Fatalf("replaced symlink backup = %q, %v", target, err)
	}
	if got := readKVDB(t, filepath.Join(env.targetHome, "memories_1.sqlite")); got != "source-memory" {
		t.Fatalf("memory database was not migrated: %q", got)
	}
	config := readFile(t, filepath.Join(env.targetHome, "config.toml"))
	if strings.Contains(config, filepath.Dir(env.sourceHome)) || !strings.Contains(config, env.targetProject) || !strings.Contains(config, filepath.Join(filepath.Dir(env.targetHome), ".local", "bin", "tool")) {
		t.Fatalf("config paths were not rewritten: %s", config)
	}
	linkedSkill := filepath.Join(env.targetHome, "skills", "linked-skill")
	if target, err := os.Readlink(linkedSkill); err != nil || target != filepath.Join(env.targetProject, "skills", "linked-skill") {
		t.Fatalf("linked skill target = %q, %v", target, err)
	}
	if got := readFile(t, filepath.Join(linkedSkill, "SKILL.md")); got != "linked skill\n" {
		t.Fatalf("linked skill content = %q", got)
	}
	state := readJSONMap(t, filepath.Join(env.targetHome, ".codex-global-state.json"))
	if state["electron-local-remote-control-installation-id"] != "target-installation" {
		t.Fatalf("target machine identity changed: %#v", state)
	}
	if state["electron-remote-control-client-enrollments"] == nil {
		t.Fatalf("target remote enrollment was removed: %#v", state)
	}
	verify, err := Verify(VerifyOptions{BundleDir: env.bundle, Home: env.targetHome})
	if err != nil {
		t.Fatal(err)
	}
	if verify.Status != "ok" {
		t.Fatalf("verification failed: %#v", verify)
	}
}

func TestProfileExportBlocksHighConfidenceSecret(t *testing.T) {
	env := newMigrationTestEnv(t)
	writeFile(t, filepath.Join(env.sourceHome, "config.toml"), `api_key = "sk-abcdefghijklmnopqrstuvwxyz123456"`)
	_, err := Export(ExportOptions{Home: env.sourceHome, TargetHome: env.targetHome, TargetWorkspace: env.targetWorkspace, Projects: []string{env.sourceProject}, Out: env.bundle})
	if err == nil || !strings.Contains(err.Error(), "secret scan blocked") {
		t.Fatalf("expected secret scan failure, got %v", err)
	}
}

func TestDiscoverReadsCodexProjectState(t *testing.T) {
	env := newMigrationTestEnv(t)
	result, err := Discover(DiscoverOptions{Home: env.sourceHome})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Projects) != 1 {
		t.Fatalf("projects = %#v", result.Projects)
	}
	project := result.Projects[0]
	if project.Name != "project-a" || project.Path != env.sourceProject || project.Threads != 1 || project.GitRepos != 1 || !project.AllClean {
		t.Fatalf("unexpected candidate: %#v", project)
	}
}

func TestFetchIsIncremental(t *testing.T) {
	env := newMigrationTestEnv(t)
	if _, err := Export(ExportOptions{Home: env.sourceHome, TargetHome: env.targetHome, TargetWorkspace: env.targetWorkspace, Projects: []string{env.sourceProject}, Out: env.bundle}); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(env.bundle, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve() }()
	defer server.Close(context.Background())
	target := filepath.Join(t.TempDir(), "fetched")
	first, err := Fetch(FetchOptions{Source: server.URLs[0], Out: target})
	if err != nil {
		t.Fatal(err)
	}
	if first.DownloadedFiles == 0 || first.ReusedFiles != 0 {
		t.Fatalf("unexpected first fetch: %#v", first)
	}
	manifest, err := readManifest(target)
	if err != nil {
		t.Fatal(err)
	}
	missing, err := safeJoin(target, manifest.Threads[0].BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	second, err := Fetch(FetchOptions{Source: server.URLs[0], Out: target})
	if err != nil {
		t.Fatal(err)
	}
	if second.DownloadedFiles != 1 || second.ReusedFiles != first.DownloadedFiles-1 {
		t.Fatalf("fetch was not incremental: first=%#v second=%#v", first, second)
	}
}

func TestScheduleImportUsesOneShotLaunchAgent(t *testing.T) {
	env := newMigrationTestEnv(t)
	if _, err := Export(ExportOptions{Home: env.sourceHome, TargetHome: env.targetHome, TargetWorkspace: env.targetWorkspace, Projects: []string{env.sourceProject}, Out: env.bundle}); err != nil {
		t.Fatal(err)
	}
	result, err := ScheduleImport(ScheduleOptions{BundleDir: env.bundle, Home: env.targetHome, CLIPath: "/usr/local/bin/capsule"})
	if err != nil {
		t.Fatal(err)
	}
	plist := readFile(t, result.PlistPath)
	if !strings.Contains(plist, "<key>KeepAlive</key><false/>") {
		t.Fatalf("LaunchAgent is not one-shot:\n%s", plist)
	}
	if strings.Contains(result.StagingDir, "Downloads") || !strings.HasPrefix(result.StagingDir, env.targetHome+string(filepath.Separator)) {
		t.Fatalf("unsafe staging dir: %s", result.StagingDir)
	}
	runner := readFile(t, result.RunnerPath)
	if !strings.Contains(runner, "profile verify") || !strings.Contains(runner, "tell application \"ChatGPT\" to quit") || !strings.Contains(runner, "PLIST_PATH="+quoteShell(result.PlistPath)) || !strings.Contains(runner, "trap cleanup EXIT") {
		t.Fatalf("runner is incomplete:\n%s", runner)
	}
	if output, err := exec.Command("bash", "-n", result.RunnerPath).CombinedOutput(); err != nil {
		t.Fatalf("runner shell syntax: %v: %s", err, output)
	}
}

func TestGitBundleFallbackIsFetchedLazily(t *testing.T) {
	env := newMigrationTestEnv(t)
	if _, err := Export(ExportOptions{Home: env.sourceHome, TargetHome: env.targetHome, TargetWorkspace: env.targetWorkspace, Projects: []string{env.sourceProject}, Out: env.bundle, GitBundleFallback: true}); err != nil {
		t.Fatal(err)
	}
	fetched := filepath.Join(t.TempDir(), "fetched")
	withoutBundles, err := Fetch(FetchOptions{Source: env.bundle, Out: fetched})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := readManifest(fetched)
	if err != nil {
		t.Fatal(err)
	}
	repo := manifest.Projects[0].Repos[0]
	bundlePath, err := safeJoin(fetched, repo.BundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(bundlePath); !os.IsNotExist(err) {
		t.Fatalf("Git bundle should not be fetched by default: %v", err)
	}
	withBundles, err := Fetch(FetchOptions{Source: env.bundle, Out: fetched, IncludeGitBundles: true})
	if err != nil {
		t.Fatal(err)
	}
	if withBundles.DownloadedFiles != 1 || withBundles.ReusedFiles != withoutBundles.DownloadedFiles {
		t.Fatalf("unexpected fallback fetch: before=%#v after=%#v", withoutBundles, withBundles)
	}
	if _, err := CloneProjects(CloneOptions{BundleDir: fetched, Execute: true}); err != nil {
		t.Fatal(err)
	}
	if !isGitRepo(env.targetProject) {
		t.Fatalf("target was not cloned from fallback bundle")
	}
}

type migrationTestEnv struct {
	sourceHome      string
	targetHome      string
	sourceProject   string
	targetProject   string
	targetWorkspace string
	bundle          string
	threadID        string
}

func newMigrationTestEnv(t *testing.T) migrationTestEnv {
	t.Helper()
	root := t.TempDir()
	env := migrationTestEnv{
		sourceHome:      filepath.Join(root, "source", ".codex"),
		targetHome:      filepath.Join(root, "target", ".codex"),
		sourceProject:   filepath.Join(root, "source", "workspace", "project-a"),
		targetWorkspace: filepath.Join(root, "target", "workspace"),
		bundle:          filepath.Join(root, "bundle"),
		threadID:        "019f0000-0000-7000-8000-000000000001",
	}
	env.targetProject = filepath.Join(env.targetWorkspace, "project-a")
	for _, dir := range []string{env.sourceHome, env.targetHome, env.sourceProject, env.targetWorkspace} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	initRepo(t, env.sourceProject)
	writeFile(t, filepath.Join(env.sourceProject, "skills", "linked-skill", "SKILL.md"), "linked skill\n")
	runCommand(t, "git", "-C", env.sourceProject, "add", "skills/linked-skill/SKILL.md")
	runCommand(t, "git", "-C", env.sourceProject, "commit", "-m", "add linked skill")
	sourceSession := filepath.Join(env.sourceHome, "sessions", "2026", "07", "18", "rollout-2026-07-18T00-00-00-"+env.threadID+".jsonl")
	writeFile(t, sourceSession, strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2026-07-18T00:00:00Z","type":"session_meta","payload":{"id":%q,"cwd":%q}}`, env.threadID, env.sourceProject),
		fmt.Sprintf(`{"timestamp":"2026-07-18T00:00:01Z","type":"turn_context","payload":{"cwd":%q}}`, env.sourceProject),
		`{"timestamp":"2026-07-18T00:00:02Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep source path in visible conversation"}]}}`,
	}, "\n")+"\n")
	createThreadsDB(t, filepath.Join(env.sourceHome, "state_5.sqlite"), env.threadID, env.sourceProject, sourceSession)
	createThreadsDB(t, filepath.Join(env.targetHome, "state_5.sqlite"), "target-empty-thread", filepath.Join(root, "target", "empty"), filepath.Join(env.targetHome, "sessions", "empty.jsonl"))
	writeJSONLine(t, filepath.Join(env.sourceHome, "session_index.jsonl"), map[string]any{"id": env.threadID, "thread_name": "Migrated thread", "updated_at": "2026-07-18T00:00:02Z"})
	writeJSONLine(t, filepath.Join(env.targetHome, "session_index.jsonl"), map[string]any{"id": "target-empty-thread", "thread_name": "Empty", "updated_at": "2026-07-18T00:00:00Z"})
	writeFile(t, filepath.Join(env.sourceHome, "config.toml"), "project = \""+env.sourceProject+"\"\nruntime = \""+filepath.Join(filepath.Dir(env.sourceHome), ".local", "bin", "tool")+"\"\n")
	writeFile(t, filepath.Join(env.sourceHome, "AGENTS.md"), "source instructions\n")
	writeFile(t, filepath.Join(env.sourceHome, "skills", "user-skill", "SKILL.md"), "user skill\n")
	if err := os.Symlink(filepath.Join(env.sourceProject, "skills", "linked-skill"), filepath.Join(env.sourceHome, "skills", "linked-skill")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(env.sourceHome, "skills", ".system", "managed.md"), "source managed\n")
	writeFile(t, filepath.Join(env.sourceHome, "auth.json"), "source-auth")
	createKVDB(t, filepath.Join(env.sourceHome, "memories_1.sqlite"), "source-memory")
	writeJSONTest(t, filepath.Join(env.sourceHome, ".codex-global-state.json"), map[string]any{
		"local-projects": map[string]any{
			"local-source-project": map[string]any{"id": "local-source-project", "name": "project-a", "rootPaths": []string{env.sourceProject}},
		},
	})
	writeFile(t, filepath.Join(env.targetHome, "auth.json"), "target-auth")
	writeFile(t, filepath.Join(env.targetHome, "skills", ".system", "managed.md"), "target-managed")
	writeFile(t, filepath.Join(env.targetHome, "skills", "stale", "SKILL.md"), "stale")
	if err := os.Symlink("/stale/linked-skill", filepath.Join(env.targetHome, "skills", "linked-skill")); err != nil {
		t.Fatal(err)
	}
	writeJSONTest(t, filepath.Join(env.targetHome, ".codex-global-state.json"), map[string]any{
		"electron-local-remote-control-installation-id": "target-installation",
		"electron-remote-control-client-enrollments":    []any{map[string]any{"id": "source-controller"}},
		"local-projects":                 map[string]any{},
		"thread-project-assignments":     map[string]any{},
		"thread-workspace-root-hints":    map[string]any{},
		"project-order":                  []any{},
		"electron-saved-workspace-roots": []any{},
		"projectless-thread-ids":         []any{env.threadID},
	})
	return env
}

func initRepo(t *testing.T, path string) {
	t.Helper()
	runCommand(t, "git", "-C", path, "init")
	runCommand(t, "git", "-C", path, "config", "user.email", "test@example.com")
	runCommand(t, "git", "-C", path, "config", "user.name", "Test")
	writeFile(t, filepath.Join(path, "README.md"), "test\n")
	runCommand(t, "git", "-C", path, "add", "README.md")
	runCommand(t, "git", "-C", path, "commit", "-m", "initial")
}

func cloneRepo(t *testing.T, source, target string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	runCommand(t, "git", "clone", source, target)
}

func runCommand(t *testing.T, name string, args ...string) {
	t.Helper()
	if output, err := exec.Command(name, args...).CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, output)
	}
}

func createThreadsDB(t *testing.T, path, id, cwd, rollout string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table threads (id text primary key, cwd text not null, title text not null, rollout_path text not null, updated_at integer not null default 0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`insert into threads (id, cwd, title, rollout_path, updated_at) values (?, ?, ?, ?, ?)`, id, cwd, "Migrated thread", rollout, time.Now().UnixMilli()); err != nil {
		t.Fatal(err)
	}
}

func createKVDB(t *testing.T, path, value string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`create table kv (value text); insert into kv values (?)`, value); err != nil {
		t.Fatal(err)
	}
}

func readKVDB(t *testing.T, path string) string {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var value string
	if err := db.QueryRow(`select value from kv limit 1`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func assertManifestIncludes(t *testing.T, manifest *Manifest, relative string) {
	t.Helper()
	for _, file := range manifest.ProfileFiles {
		if file.TargetRelativePath == relative {
			return
		}
	}
	t.Fatalf("manifest does not include %s", relative)
}

func assertManifestExcludes(t *testing.T, manifest *Manifest, relative string) {
	t.Helper()
	for _, file := range manifest.ProfileFiles {
		if file.TargetRelativePath == relative {
			t.Fatalf("manifest unexpectedly includes %s", relative)
		}
	}
}

func writeFile(t *testing.T, path, value string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeJSONLine(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data)+"\n")
}

func writeJSONTest(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, string(data)+"\n")
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func readJSONMap(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}
