package profile

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

func Export(opts ExportOptions) (*ExportResult, error) {
	home, err := resolveHome(opts.Home)
	if err != nil {
		return nil, err
	}
	if len(opts.Projects) == 0 {
		return nil, errors.New("at least one --project is required; migration scope must be explicit")
	}
	if opts.TargetWorkspace == "" {
		return nil, errors.New("missing --target-workspace")
	}
	targetWorkspace, err := filepath.Abs(opts.TargetWorkspace)
	if err != nil {
		return nil, err
	}
	targetHome := opts.TargetHome
	if targetHome == "" {
		targetHome = filepath.Join(filepath.Dir(targetWorkspace), ".codex")
	}
	projects, err := describeProjects(opts.Projects, targetWorkspace)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	out := opts.Out
	if out == "" {
		out = filepath.Join(home, "profile-migrations", id)
	}
	out, err = filepath.Abs(out)
	if err != nil {
		return nil, err
	}
	if entries, readErr := os.ReadDir(out); readErr == nil && len(entries) > 0 {
		return nil, fmt.Errorf("output directory is not empty: %s", out)
	} else if readErr != nil && !isNotExist(readErr) {
		return nil, readErr
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		return nil, err
	}
	profileFiles, err := exportProfileFiles(home, out, opts.UnsafeIncludeSecrets)
	if err != nil {
		return nil, err
	}
	threads, err := exportThreads(home, out, projects)
	if err != nil {
		return nil, err
	}
	if opts.GitBundleFallback {
		if err := exportGitBundles(out, projects); err != nil {
			return nil, err
		}
	}
	manifest := &Manifest{
		Schema:          Schema,
		ID:              id,
		CreatedAt:       time.Now().UTC(),
		SourceHome:      home,
		TargetHome:      targetHome,
		SourceUserHome:  filepath.Dir(home),
		TargetUserHome:  filepath.Dir(targetHome),
		SourceWorkspace: commonProjectParent(projects),
		TargetWorkspace: targetWorkspace,
		Projects:        projects,
		Threads:         threads,
		ProfileFiles:    profileFiles,
		Exclusions:      append([]string(nil), DefaultExclusions...),
	}
	manifest.Stats.Projects = len(projects)
	manifest.Stats.Threads = len(threads)
	manifest.Stats.ProfileFiles = len(profileFiles)
	for _, project := range projects {
		manifest.Stats.GitRepos += len(project.Repos)
		for _, repo := range project.Repos {
			manifest.Stats.Bytes += repo.BundleBytes
		}
	}
	for _, file := range profileFiles {
		manifest.Stats.Bytes += file.Bytes
	}
	for _, thread := range threads {
		manifest.Stats.Bytes += thread.Bytes
	}
	manifestPath := filepath.Join(out, "manifest.json")
	if err := writeJSON(manifestPath, manifest); err != nil {
		return nil, err
	}
	return &ExportResult{
		Status:       "ok",
		BundleDir:    out,
		ManifestPath: manifestPath,
		Manifest:     manifest,
		ClonePlan:    BuildClonePlan(manifest),
	}, nil
}

func exportGitBundles(out string, projects []Project) error {
	for projectIndex := range projects {
		project := &projects[projectIndex]
		for repoIndex := range project.Repos {
			repo := &project.Repos[repoIndex]
			name := project.ID
			if repo.RelativePath != "" {
				name += "-" + strings.NewReplacer("/", "-", "\\", "-").Replace(repo.RelativePath)
			}
			relative := filepath.ToSlash(filepath.Join("git-bundles", name+".bundle"))
			target, err := safeJoin(out, relative)
			if err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			repoRoot := filepath.Join(project.SourcePath, filepath.FromSlash(repo.RelativePath))
			if output, err := exec.Command("git", "-C", repoRoot, "bundle", "create", target, "--all").CombinedOutput(); err != nil {
				return fmt.Errorf("create Git bundle for %s: %w: %s", project.Name, err, output)
			}
			hash, size, err := fileHash(target)
			if err != nil {
				return err
			}
			repo.BundlePath = relative
			repo.BundleSHA256 = hash
			repo.BundleBytes = size
		}
	}
	return nil
}

func describeProjects(paths []string, targetWorkspace string) ([]Project, error) {
	seen := map[string]bool{}
	seenTargets := map[string]bool{}
	projects := make([]Project, 0, len(paths))
	for _, path := range paths {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(absolute)
		if err != nil {
			return nil, fmt.Errorf("project %s: %w", absolute, err)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("project is not a directory: %s", absolute)
		}
		if seen[absolute] {
			continue
		}
		seen[absolute] = true
		name := filepath.Base(absolute)
		targetPath := filepath.Join(targetWorkspace, name)
		if seenTargets[targetPath] {
			return nil, fmt.Errorf("multiple project roots map to the same target path: %s", targetPath)
		}
		seenTargets[targetPath] = true
		project := Project{
			ID:         name,
			Name:       name,
			SourcePath: absolute,
			TargetPath: targetPath,
		}
		project.Repos, err = discoverGitRepos(project)
		if err != nil {
			return nil, err
		}
		projects = append(projects, project)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].SourcePath < projects[j].SourcePath })
	return projects, nil
}

func discoverGitRepos(project Project) ([]GitRepo, error) {
	var roots []string
	err := filepath.WalkDir(project.SourcePath, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(project.SourcePath, path)
		if err != nil {
			return err
		}
		depth := 0
		if rel != "." {
			depth = len(strings.Split(filepath.ToSlash(rel), "/"))
		}
		if depth > 2 {
			return filepath.SkipDir
		}
		if entry.Name() == ".git" {
			roots = append(roots, filepath.Dir(path))
			return filepath.SkipDir
		}
		if entry.Name() == "node_modules" || entry.Name() == "vendor" || entry.Name() == ".cache" {
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var repos []GitRepo
	for _, root := range roots {
		rel, _ := filepath.Rel(project.SourcePath, root)
		if rel == "." {
			rel = ""
		}
		repo := GitRepo{
			RelativePath: filepath.ToSlash(rel),
			TargetPath:   filepath.Join(project.TargetPath, rel),
			Branch:       gitOutput(root, "branch", "--show-current"),
			Commit:       gitOutput(root, "rev-parse", "HEAD"),
			Remotes:      map[string]string{},
			Clean:        gitOutput(root, "status", "--porcelain") == "",
		}
		remoteLines := strings.Split(gitOutput(root, "remote", "-v"), "\n")
		for _, line := range remoteLines {
			fields := strings.Fields(line)
			if len(fields) >= 3 && fields[2] == "(fetch)" {
				repo.Remotes[fields[0]] = fields[1]
			}
		}
		repos = append(repos, repo)
	}
	sort.Slice(repos, func(i, j int) bool { return repos[i].RelativePath < repos[j].RelativePath })
	return repos, nil
}

func gitOutput(root string, args ...string) string {
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	data, err := command.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func exportProfileFiles(home, out string, unsafe bool) ([]File, error) {
	var result []File
	for _, relative := range DefaultProfileDatabases {
		source := filepath.Join(home, relative)
		if _, err := os.Stat(source); isNotExist(err) {
			continue
		} else if err != nil {
			return nil, err
		}
		bundleRel := filepath.ToSlash(filepath.Join("profile", relative))
		target, err := safeJoin(out, bundleRel)
		if err != nil {
			return nil, err
		}
		if err := snapshotSQLite(source, target); err != nil {
			return nil, fmt.Errorf("snapshot %s: %w", relative, err)
		}
		hash, size, err := fileHash(target)
		if err != nil {
			return nil, err
		}
		result = append(result, File{TargetRelativePath: relative, BundlePath: bundleRel, SHA256: hash, Bytes: size, Mode: 0o600})
	}
	for _, relative := range DefaultProfilePaths {
		source := filepath.Join(home, relative)
		info, err := os.Lstat(source)
		if isNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(source)
			if err != nil {
				return nil, err
			}
			result = append(result, File{TargetRelativePath: relative, Mode: uint32(os.ModeSymlink), LinkTarget: linkTarget})
			continue
		}
		if info.Mode().IsRegular() {
			if !unsafe {
				if err := scanFileForSecrets(source); err != nil {
					return nil, err
				}
			}
			bundleRel := filepath.ToSlash(filepath.Join("profile", relative))
			target, err := safeJoin(out, bundleRel)
			if err != nil {
				return nil, err
			}
			hash, size, err := copyAndHash(source, target, info.Mode())
			if err != nil {
				return nil, err
			}
			result = append(result, File{TargetRelativePath: relative, BundlePath: bundleRel, SHA256: hash, Bytes: size, Mode: uint32(info.Mode().Perm())})
			continue
		}
		files, err := copyTreeFiles(source, out, relative, unsafe)
		if err != nil {
			return nil, err
		}
		result = append(result, files...)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].TargetRelativePath < result[j].TargetRelativePath })
	return result, nil
}

func snapshotSQLite(source, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	_ = os.Remove(target)
	db, err := sql.Open("sqlite", source)
	if err != nil {
		return err
	}
	defer db.Close()
	escaped := strings.ReplaceAll(target, "'", "''")
	_, err = db.Exec("VACUUM INTO '" + escaped + "'")
	return err
}

func exportThreads(home, out string, projects []Project) ([]Thread, error) {
	dbPath := filepath.Join(home, "state_5.sqlite")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("select * from threads")
	if err != nil {
		return nil, fmt.Errorf("read Codex threads: %w", err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	index, err := readSessionIndex(filepath.Join(home, "session_index.jsonl"))
	if err != nil && !isNotExist(err) {
		return nil, err
	}
	paths, err := mapSessionPaths(home)
	if err != nil {
		return nil, err
	}
	var threads []Thread
	for rows.Next() {
		row, err := scanRow(rows, columns)
		if err != nil {
			return nil, err
		}
		cwd := stringValue(row["cwd"])
		project, ok := pathWithin(cwd, projects)
		if !ok {
			continue
		}
		id := stringValue(row["id"])
		sourceSession := stringValue(row["rollout_path"])
		if sourceSession == "" {
			sourceSession = paths[id]
		}
		if sourceSession == "" {
			return nil, fmt.Errorf("session file not found for selected thread %s", id)
		}
		rel, err := filepath.Rel(home, sourceSession)
		if err != nil || strings.HasPrefix(rel, "..") {
			return nil, fmt.Errorf("thread %s session is outside Codex home: %s", id, sourceSession)
		}
		bundleRel := filepath.ToSlash(filepath.Join("sessions", id+".jsonl"))
		bundlePath, err := safeJoin(out, bundleRel)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(sourceSession)
		if err != nil {
			return nil, err
		}
		hash, size, err := copyAndHash(sourceSession, bundlePath, info.Mode())
		if err != nil {
			return nil, err
		}
		targetCWD := replaceRoot(cwd, project.SourcePath, project.TargetPath)
		targetRel := filepath.ToSlash(rel)
		row["cwd"] = targetCWD
		row["rollout_path"] = filepath.Join(home, filepath.FromSlash(targetRel))
		entry := index[id]
		threads = append(threads, Thread{
			ID:                    id,
			Title:                 firstNonEmpty(stringValue(row["title"]), stringValue(entry["thread_name"])),
			SourceCWD:             cwd,
			TargetCWD:             targetCWD,
			SourceSessionPath:     sourceSession,
			TargetSessionRelative: targetRel,
			BundlePath:            bundleRel,
			SHA256:                hash,
			Bytes:                 size,
			Row:                   row,
			IndexEntry:            entry,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(threads, func(i, j int) bool { return threads[i].ID < threads[j].ID })
	return threads, nil
}

func scanRow(rows *sql.Rows, columns []string) (map[string]any, error) {
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for i := range values {
		pointers[i] = &values[i]
	}
	if err := rows.Scan(pointers...); err != nil {
		return nil, err
	}
	result := make(map[string]any, len(columns))
	for i, column := range columns {
		if bytes, ok := values[i].([]byte); ok {
			result[column] = string(bytes)
		} else {
			result[column] = values[i]
		}
	}
	return result, nil
}

func readSessionIndex(path string) (map[string]map[string]any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	result := map[string]map[string]any{}
	for decoder.More() {
		var entry map[string]any
		if err := decoder.Decode(&entry); err != nil {
			return nil, err
		}
		if id := stringValue(entry["id"]); id != "" {
			result[id] = entry
		}
	}
	return result, nil
}

func mapSessionPaths(home string) (map[string]string, error) {
	result := map[string]string{}
	for _, name := range []string{"sessions", "archived_sessions"} {
		root := filepath.Join(home, name)
		if _, err := os.Stat(root); isNotExist(err) {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
				return nil
			}
			base := strings.TrimSuffix(entry.Name(), ".jsonl")
			if len(base) >= 36 {
				candidate := base[len(base)-36:]
				if _, err := uuid.Parse(candidate); err == nil {
					result[candidate] = path
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func commonProjectParent(projects []Project) string {
	if len(projects) == 0 {
		return ""
	}
	parent := filepath.Dir(projects[0].SourcePath)
	for _, project := range projects[1:] {
		for parent != string(filepath.Separator) && project.SourcePath != parent && !strings.HasPrefix(project.SourcePath, parent+string(filepath.Separator)) {
			parent = filepath.Dir(parent)
		}
	}
	return parent
}

func BuildClonePlan(manifest *Manifest) []CloneAction {
	var actions []CloneAction
	for _, project := range manifest.Projects {
		if len(project.Repos) == 0 {
			actions = append(actions, CloneAction{Project: project.Name, TargetPath: project.TargetPath, NeedsBundle: true, BundleReason: "project root has no Git repository"})
			continue
		}
		for _, repo := range project.Repos {
			remote := repo.Remotes["origin"]
			if remote == "" {
				for _, value := range repo.Remotes {
					remote = value
					break
				}
			}
			action := CloneAction{Project: project.Name, TargetPath: repo.TargetPath, Remote: remote, Branch: repo.Branch, Commit: repo.Commit}
			if remote == "" && repo.BundlePath == "" {
				action.NeedsBundle = true
				action.BundleReason = "repository has no fetch remote"
			} else if remote != "" {
				action.Command = fmt.Sprintf("git clone %s %s && git -C %s checkout %s", quoteShell(remote), quoteShell(repo.TargetPath), quoteShell(repo.TargetPath), quoteShell(repo.Commit))
			} else {
				action.Command = fmt.Sprintf("git clone %s %s && git -C %s checkout %s", quoteShell(repo.BundlePath), quoteShell(repo.TargetPath), quoteShell(repo.TargetPath), quoteShell(repo.Commit))
			}
			actions = append(actions, action)
		}
	}
	return actions
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
