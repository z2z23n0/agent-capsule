package profile

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

func Import(opts ImportOptions) (*ImportResult, error) {
	bundleDir, err := filepath.Abs(opts.BundleDir)
	if err != nil {
		return nil, err
	}
	manifest, err := readManifest(bundleDir)
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
	missing := missingProjectRepos(manifest)
	result := &ImportResult{
		Status:            "planned",
		DryRun:            !opts.Execute,
		TargetHome:        home,
		Projects:          len(manifest.Projects),
		Threads:           len(manifest.Threads),
		ProfileFiles:      len(manifest.ProfileFiles),
		MissingProjects:   missing,
		PreservedIdentity: true,
	}
	for _, file := range manifest.ProfileFiles {
		join := safeJoin
		if file.LinkTarget != "" {
			join = safeJoinAllowFinalSymlink
		}
		target, joinErr := join(home, file.TargetRelativePath)
		if joinErr != nil {
			return nil, joinErr
		}
		result.Writes = append(result.Writes, target)
	}
	for _, thread := range manifest.Threads {
		target, joinErr := safeJoin(home, thread.TargetSessionRelative)
		if joinErr != nil {
			return nil, joinErr
		}
		result.Writes = append(result.Writes, target)
	}
	result.Writes = append(result.Writes,
		filepath.Join(home, "state_5.sqlite"),
		filepath.Join(home, "session_index.jsonl"),
		filepath.Join(home, ".codex-global-state.json"),
	)
	sort.Strings(result.Writes)
	if !opts.Execute {
		return result, nil
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("project repositories must be cloned before import: %s", strings.Join(missing, ", "))
	}
	if opts.RequireStopped {
		if running, err := codexAppRunning(); err != nil {
			return nil, err
		} else if running {
			return nil, errors.New("Codex App is running; quit it before profile import or use profile schedule-import")
		}
	}
	if _, err := os.Stat(filepath.Join(home, "state_5.sqlite")); err != nil {
		return nil, fmt.Errorf("target Codex database is missing; open Codex once before importing: %w", err)
	}
	if err := validateBundleFiles(bundleDir, manifest); err != nil {
		return nil, err
	}
	if err := checkpointSQLite(filepath.Join(home, "state_5.sqlite")); err != nil {
		return nil, err
	}
	backupDir, err := backupTarget(home, manifest)
	if err != nil {
		return nil, err
	}
	result.BackupDir = backupDir
	if err := importProfileFiles(bundleDir, home, manifest); err != nil {
		return nil, err
	}
	if err := importSessions(bundleDir, home, manifest); err != nil {
		return nil, err
	}
	if err := upsertThreads(filepath.Join(home, "state_5.sqlite"), manifest.Threads, home); err != nil {
		return nil, err
	}
	if err := mergeSessionIndex(home, manifest.Threads); err != nil {
		return nil, err
	}
	if err := mergeGlobalState(home, manifest); err != nil {
		return nil, err
	}
	if err := checkpointSQLite(filepath.Join(home, "state_5.sqlite")); err != nil {
		return nil, err
	}
	result.Status = "ok"
	return result, nil
}

func validateBundleFiles(bundleDir string, manifest *Manifest) error {
	for _, file := range manifest.ProfileFiles {
		if file.LinkTarget != "" {
			continue
		}
		path, err := safeJoin(bundleDir, file.BundlePath)
		if err != nil {
			return err
		}
		if !fileMatches(path, file.SHA256, file.Bytes) {
			return fmt.Errorf("profile bundle file is missing or corrupt: %s", file.BundlePath)
		}
	}
	for _, thread := range manifest.Threads {
		path, err := safeJoin(bundleDir, thread.BundlePath)
		if err != nil {
			return err
		}
		if !fileMatches(path, thread.SHA256, thread.Bytes) {
			return fmt.Errorf("profile session is missing or corrupt: %s", thread.BundlePath)
		}
	}
	return nil
}

func missingProjectRepos(manifest *Manifest) []string {
	var missing []string
	for _, project := range manifest.Projects {
		if len(project.Repos) == 0 {
			missing = append(missing, project.TargetPath)
			continue
		}
		for _, repo := range project.Repos {
			if !isGitRepo(repo.TargetPath) {
				missing = append(missing, repo.TargetPath)
			}
		}
	}
	sort.Strings(missing)
	return missing
}

func codexAppRunning() (bool, error) {
	command := exec.Command("pgrep", "-x", "ChatGPT")
	err := command.Run()
	if err == nil {
		return true, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return false, nil
	}
	return false, err
}

func checkpointSQLite(path string) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer db.Close()
	var busy, logFrames, checkpointed int
	if err := db.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint Codex database: %w", err)
	}
	if busy != 0 {
		return errors.New("Codex database remained busy during WAL checkpoint")
	}
	return nil
}

func backupTarget(home string, manifest *Manifest) (string, error) {
	backup := filepath.Join(home, "backups_state", "agent-capsule-profile", time.Now().UTC().Format("20060102T150405Z")+"-"+manifest.ID)
	if err := os.MkdirAll(backup, 0o700); err != nil {
		return "", err
	}
	seen := map[string]bool{}
	backupPath := func(relative string) error {
		if seen[relative] {
			return nil
		}
		seen[relative] = true
		source, err := safeJoin(home, relative)
		if err != nil {
			return err
		}
		info, err := os.Stat(source)
		if isNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		target, err := safeJoin(backup, relative)
		if err != nil {
			return err
		}
		return copyFile(source, target, info.Mode())
	}
	for _, relative := range []string{"state_5.sqlite", "state_5.sqlite-wal", "state_5.sqlite-shm", "session_index.jsonl", ".codex-global-state.json"} {
		if err := backupPath(relative); err != nil {
			return "", err
		}
	}
	for _, file := range manifest.ProfileFiles {
		if file.LinkTarget == "" {
			if err := backupPath(file.TargetRelativePath); err != nil {
				return "", err
			}
		}
		if strings.HasSuffix(file.TargetRelativePath, ".sqlite") {
			if err := backupPath(file.TargetRelativePath + "-wal"); err != nil {
				return "", err
			}
			if err := backupPath(file.TargetRelativePath + "-shm"); err != nil {
				return "", err
			}
		}
	}
	if err := backupManagedProfileTrees(home, backup); err != nil {
		return "", err
	}
	for _, thread := range manifest.Threads {
		if err := backupPath(thread.TargetSessionRelative); err != nil {
			return "", err
		}
	}
	return backup, nil
}

func importProfileFiles(bundleDir, home string, manifest *Manifest) error {
	if err := pruneManagedProfileTrees(home, manifest); err != nil {
		return err
	}
	for _, file := range manifest.ProfileFiles {
		target, err := safeJoin(home, file.TargetRelativePath)
		if err != nil {
			return err
		}
		if file.LinkTarget != "" {
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			linkTarget := string(rewriteProfileText([]byte(file.LinkTarget), manifest, home))
			if err := os.Symlink(linkTarget, target); err != nil {
				return err
			}
			continue
		}
		source, err := safeJoin(bundleDir, file.BundlePath)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(source)
		if err != nil {
			return err
		}
		if utf8.Valid(data) {
			data = rewriteProfileText(data, manifest, home)
		}
		if err := writeBytes(target, data, os.FileMode(file.Mode)); err != nil {
			return err
		}
		if strings.HasSuffix(file.TargetRelativePath, ".sqlite") {
			_ = os.Remove(target + "-wal")
			_ = os.Remove(target + "-shm")
		}
	}
	return nil
}

func backupManagedProfileTrees(home, backup string) error {
	for _, root := range managedProfileDirs() {
		source := filepath.Join(home, root)
		if _, err := os.Stat(source); isNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		err := filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relHome, err := filepath.Rel(home, path)
			if err != nil {
				return err
			}
			if filepath.ToSlash(relHome) == "skills/.system" && entry.IsDir() {
				return filepath.SkipDir
			}
			if entry.IsDir() {
				return nil
			}
			target, err := safeJoin(backup, filepath.ToSlash(relHome))
			if err != nil {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				linkTarget, err := os.Readlink(path)
				if err != nil {
					return err
				}
				if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
					return err
				}
				return os.Symlink(linkTarget, target)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}
			return copyFile(path, target, info.Mode())
		})
		if err != nil {
			return err
		}
	}
	return nil
}

func pruneManagedProfileTrees(home string, manifest *Manifest) error {
	desired := map[string]bool{}
	desiredLinks := map[string]bool{}
	for _, file := range manifest.ProfileFiles {
		relative := filepath.Clean(filepath.FromSlash(file.TargetRelativePath))
		desired[relative] = true
		if file.LinkTarget != "" {
			desiredLinks[relative] = true
		}
	}
	for _, root := range managedProfileDirs() {
		rootPath := filepath.Join(home, root)
		if _, err := os.Stat(rootPath); isNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		var paths []string
		err := filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(home, path)
			if err != nil {
				return err
			}
			if filepath.ToSlash(rel) == "skills/.system" && entry.IsDir() {
				return filepath.SkipDir
			}
			if path != rootPath {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			return err
		}
		sort.Slice(paths, func(i, j int) bool { return len(paths[i]) > len(paths[j]) })
		for _, path := range paths {
			rel, _ := filepath.Rel(home, path)
			info, err := os.Lstat(path)
			if isNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			if info.IsDir() {
				_ = os.Remove(path)
				continue
			}
			cleanRel := filepath.Clean(rel)
			if desiredLinks[cleanRel] || !desired[cleanRel] {
				if err := os.Remove(path); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func managedProfileDirs() []string {
	return []string{"rules", "skills", "memories", "data", "automations"}
}

func rewriteProfileText(data []byte, manifest *Manifest, targetHome string) []byte {
	text := string(data)
	targetUserHome := manifest.TargetUserHome
	if targetUserHome == "" {
		targetUserHome = filepath.Dir(targetHome)
	}
	replacements := [][2]string{{manifest.SourceHome, targetHome}}
	if manifest.SourceUserHome != "" {
		replacements = append(replacements, [2]string{manifest.SourceUserHome, targetUserHome})
	}
	if manifest.SourceWorkspace != "" && manifest.TargetWorkspace != "" {
		replacements = append(replacements, [2]string{manifest.SourceWorkspace, manifest.TargetWorkspace})
	}
	for _, project := range manifest.Projects {
		replacements = append(replacements, [2]string{project.SourcePath, project.TargetPath})
	}
	sort.SliceStable(replacements, func(i, j int) bool { return len(replacements[i][0]) > len(replacements[j][0]) })
	for _, replacement := range replacements {
		text = strings.ReplaceAll(text, replacement[0], replacement[1])
	}
	return []byte(text)
}

func importSessions(bundleDir, home string, manifest *Manifest) error {
	for _, thread := range manifest.Threads {
		source, err := safeJoin(bundleDir, thread.BundlePath)
		if err != nil {
			return err
		}
		target, err := safeJoin(home, thread.TargetSessionRelative)
		if err != nil {
			return err
		}
		if err := rewriteSession(source, target, manifest, home); err != nil {
			return fmt.Errorf("rewrite session %s: %w", thread.ID, err)
		}
	}
	return nil
}

func rewriteSession(source, target string, manifest *Manifest, targetHome string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	tmp := target + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	writer := bufio.NewWriter(out)
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var event map[string]any
		if err := json.Unmarshal(line, &event); err != nil {
			out.Close()
			return err
		}
		typeName := stringValue(event["type"])
		if typeName == "session_meta" || typeName == "turn_context" {
			rewriteJSONStrings(event["payload"], manifest, targetHome)
		} else {
			if _, err := writer.Write(append(append([]byte(nil), line...), '\n')); err != nil {
				out.Close()
				return err
			}
			continue
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			out.Close()
			return err
		}
		if _, err := writer.Write(append(encoded, '\n')); err != nil {
			out.Close()
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		out.Close()
		return err
	}
	if err := writer.Flush(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, target)
}

func rewriteJSONStrings(value any, manifest *Manifest, targetHome string) {
	switch item := value.(type) {
	case map[string]any:
		for key, child := range item {
			if text, ok := child.(string); ok {
				item[key] = string(rewriteProfileText([]byte(text), manifest, targetHome))
			} else {
				rewriteJSONStrings(child, manifest, targetHome)
			}
		}
	case []any:
		for index, child := range item {
			if text, ok := child.(string); ok {
				item[index] = string(rewriteProfileText([]byte(text), manifest, targetHome))
			} else {
				rewriteJSONStrings(child, manifest, targetHome)
			}
		}
	}
}

func upsertThreads(dbPath string, threads []Thread, home string) error {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	columns, err := tableColumns(db, "threads")
	if err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, thread := range threads {
		row := cloneMap(thread.Row)
		row["id"] = thread.ID
		row["cwd"] = thread.TargetCWD
		targetPath, err := safeJoin(home, thread.TargetSessionRelative)
		if err != nil {
			return err
		}
		row["rollout_path"] = targetPath
		var names []string
		for _, column := range columns {
			if _, ok := row[column]; ok {
				names = append(names, column)
			}
		}
		if len(names) == 0 {
			return errors.New("target threads table has no compatible columns")
		}
		placeholders := make([]string, len(names))
		updates := make([]string, 0, len(names)-1)
		values := make([]any, len(names))
		for i, name := range names {
			placeholders[i] = "?"
			values[i] = row[name]
			if name != "id" {
				updates = append(updates, fmt.Sprintf("%s=excluded.%s", quoteIdentifier(name), quoteIdentifier(name)))
			}
		}
		query := fmt.Sprintf("insert into threads (%s) values (%s) on conflict(id) do update set %s", joinIdentifiers(names), strings.Join(placeholders, ","), strings.Join(updates, ","))
		if _, err := tx.Exec(query, values...); err != nil {
			return fmt.Errorf("upsert thread %s: %w", thread.ID, err)
		}
	}
	return tx.Commit()
}

func tableColumns(db *sql.DB, table string) ([]string, error) {
	rows, err := db.Query("pragma table_info(" + quoteIdentifier(table) + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		result = append(result, name)
	}
	return result, rows.Err()
}

func quoteIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func joinIdentifiers(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = quoteIdentifier(value)
	}
	return strings.Join(quoted, ",")
}

func mergeSessionIndex(home string, threads []Thread) error {
	path := filepath.Join(home, "session_index.jsonl")
	entries, err := readSessionIndex(path)
	if err != nil && !isNotExist(err) {
		return err
	}
	if entries == nil {
		entries = map[string]map[string]any{}
	}
	for _, thread := range threads {
		entry := cloneMap(thread.IndexEntry)
		entry["id"] = thread.ID
		if stringValue(entry["thread_name"]) == "" {
			entry["thread_name"] = thread.Title
		}
		entries[thread.ID] = entry
	}
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	encoder.SetEscapeHTML(false)
	for _, id := range ids {
		if err := encoder.Encode(entries[id]); err != nil {
			file.Close()
			return err
		}
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mergeGlobalState(home string, manifest *Manifest) error {
	path := filepath.Join(home, ".codex-global-state.json")
	state := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return err
		}
	} else if !isNotExist(err) {
		return err
	}
	localProjects := mapValue(state["local-projects"])
	assignments := mapValue(state["thread-project-assignments"])
	hints := mapValue(state["thread-workspace-root-hints"])
	projectOrder := stringSlice(state["project-order"])
	savedRoots := stringSlice(state["electron-saved-workspace-roots"])
	projectless := stringSlice(state["projectless-thread-ids"])
	projectlessSet := make(map[string]bool, len(projectless))
	for _, id := range projectless {
		projectlessSet[id] = true
	}
	projectIDs := map[string]string{}
	now := time.Now().UnixMilli()
	for _, project := range manifest.Projects {
		id := deterministicProjectID(project.TargetPath)
		projectIDs[project.ID] = id
		localProjects[id] = map[string]any{"id": id, "name": project.Name, "rootPaths": []string{project.TargetPath}, "createdAt": now, "updatedAt": now}
		projectOrder = appendUnique(projectOrder, id)
		savedRoots = appendUnique(savedRoots, project.TargetPath)
	}
	for _, thread := range manifest.Threads {
		project := targetProjectForPath(thread.TargetCWD, manifest.Projects)
		if project.ID == "" {
			continue
		}
		assignments[thread.ID] = map[string]any{"projectKind": "local", "projectId": projectIDs[project.ID], "path": project.TargetPath, "cwd": thread.TargetCWD, "pendingCoreUpdate": false}
		hints[thread.ID] = project.TargetPath
		delete(projectlessSet, thread.ID)
	}
	projectless = projectless[:0]
	for id := range projectlessSet {
		projectless = append(projectless, id)
	}
	sort.Strings(projectless)
	state["local-projects"] = localProjects
	state["project-order"] = projectOrder
	state["electron-saved-workspace-roots"] = savedRoots
	state["thread-project-assignments"] = assignments
	state["thread-workspace-root-hints"] = hints
	state["projectless-thread-ids"] = projectless
	return writeJSON(path, state)
}

func deterministicProjectID(path string) string {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(path))))
	return "local-" + hash[:32]
}

func targetProjectForPath(path string, projects []Project) Project {
	clean := filepath.Clean(path)
	var match Project
	for _, project := range projects {
		root := filepath.Clean(project.TargetPath)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			if len(root) > len(match.TargetPath) {
				match = project
			}
		}
	}
	return match
}

func mapValue(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return map[string]any{}
}

func stringSlice(value any) []string {
	items, ok := value.([]any)
	if !ok {
		if strings, ok := value.([]string); ok {
			return append([]string(nil), strings...)
		}
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func cloneMap(source map[string]any) map[string]any {
	result := make(map[string]any, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func writeBytes(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, mode.Perm()); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
