package profile

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

func Verify(opts VerifyOptions) (*VerifyResult, error) {
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
	result := &VerifyResult{Status: "ok", TargetHome: home, Projects: len(manifest.Projects), Threads: len(manifest.Threads), ProfileFiles: len(manifest.ProfileFiles)}
	for _, project := range manifest.Projects {
		for _, repo := range project.Repos {
			if !isGitRepo(repo.TargetPath) {
				result.Failures = append(result.Failures, "missing Git repository: "+repo.TargetPath)
			}
		}
	}
	dbPath := filepath.Join(home, "state_5.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result.DatabaseCheck); err != nil {
		result.Failures = append(result.Failures, "database integrity check failed: "+err.Error())
	} else if result.DatabaseCheck != "ok" {
		result.Failures = append(result.Failures, "database integrity check: "+result.DatabaseCheck)
	}
	for _, thread := range manifest.Threads {
		path, err := safeJoin(home, thread.TargetSessionRelative)
		if err != nil {
			result.Failures = append(result.Failures, err.Error())
			continue
		}
		if _, err := os.Stat(path); err != nil {
			result.Failures = append(result.Failures, "missing session "+thread.ID)
			continue
		}
		var cwd, rollout string
		if err := db.QueryRow("select cwd, rollout_path from threads where id = ?", thread.ID).Scan(&cwd, &rollout); err != nil {
			result.Failures = append(result.Failures, "missing database row "+thread.ID)
		} else {
			if cwd != thread.TargetCWD {
				result.Failures = append(result.Failures, fmt.Sprintf("thread %s cwd is %q, want %q", thread.ID, cwd, thread.TargetCWD))
			}
			if rollout != path {
				result.Failures = append(result.Failures, fmt.Sprintf("thread %s rollout path is %q, want %q", thread.ID, rollout, path))
			}
		}
		if err := verifySessionPaths(path, manifest, home); err != nil {
			result.Failures = append(result.Failures, fmt.Sprintf("thread %s: %v", thread.ID, err))
		}
	}
	for _, file := range manifest.ProfileFiles {
		join := safeJoin
		if file.LinkTarget != "" {
			join = safeJoinAllowFinalSymlink
		}
		path, err := join(home, file.TargetRelativePath)
		if err != nil {
			result.Failures = append(result.Failures, err.Error())
			continue
		}
		if file.LinkTarget != "" {
			actual, err := os.Readlink(path)
			if err != nil {
				result.Failures = append(result.Failures, "missing profile symlink: "+file.TargetRelativePath)
				continue
			}
			expected := string(rewriteProfileText([]byte(file.LinkTarget), manifest, home))
			if actual != expected {
				result.Failures = append(result.Failures, fmt.Sprintf("profile symlink %s points to %q, want %q", file.TargetRelativePath, actual, expected))
			}
			if _, err := os.Stat(path); err != nil {
				result.Failures = append(result.Failures, "broken profile symlink: "+file.TargetRelativePath)
			}
			continue
		}
		if _, err := os.Stat(path); err != nil {
			result.Failures = append(result.Failures, "missing profile file: "+file.TargetRelativePath)
		}
	}
	if err := verifyGlobalState(home, manifest); err != nil {
		result.Failures = append(result.Failures, err.Error())
	}
	if len(result.Failures) > 0 {
		result.Status = "failed"
	}
	return result, nil
}

func verifySessionPaths(path string, manifest *Manifest, home string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	foundTarget := false
	for scanner.Scan() {
		var event map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return err
		}
		typeName := stringValue(event["type"])
		if typeName != "session_meta" && typeName != "turn_context" {
			continue
		}
		data, _ := json.Marshal(event["payload"])
		text := string(data)
		if strings.Contains(text, manifest.SourceHome) || (manifest.SourceUserHome != "" && strings.Contains(text, manifest.SourceUserHome)) || (manifest.SourceWorkspace != "" && strings.Contains(text, manifest.SourceWorkspace)) {
			return fmt.Errorf("source path remains in session metadata")
		}
		if strings.Contains(text, home) || strings.Contains(text, manifest.TargetWorkspace) {
			foundTarget = true
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if !foundTarget {
		return fmt.Errorf("target paths were not found in session metadata")
	}
	return nil
}

func verifyGlobalState(home string, manifest *Manifest) error {
	data, err := os.ReadFile(filepath.Join(home, ".codex-global-state.json"))
	if err != nil {
		return err
	}
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	projects := mapValue(state["local-projects"])
	for _, project := range manifest.Projects {
		id := deterministicProjectID(project.TargetPath)
		if _, ok := projects[id]; !ok {
			return fmt.Errorf("global state is missing project %s", project.Name)
		}
	}
	assignments := mapValue(state["thread-project-assignments"])
	for _, thread := range manifest.Threads {
		if _, ok := assignments[thread.ID]; !ok {
			return fmt.Errorf("global state is missing thread assignment %s", thread.ID)
		}
	}
	return nil
}
