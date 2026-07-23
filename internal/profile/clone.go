package profile

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

type CloneOptions struct {
	BundleDir string
	Execute   bool
}

type CloneResult struct {
	Status  string        `json:"status"`
	DryRun  bool          `json:"dry_run"`
	Actions []CloneAction `json:"actions"`
	Cloned  int           `json:"cloned"`
	Reused  int           `json:"reused"`
}

func CloneProjects(opts CloneOptions) (*CloneResult, error) {
	manifest, err := readManifest(opts.BundleDir)
	if err != nil {
		return nil, err
	}
	actions := BuildClonePlan(manifest)
	result := &CloneResult{Status: "planned", DryRun: !opts.Execute, Actions: actions}
	if !opts.Execute {
		return result, nil
	}
	for _, action := range actions {
		if action.NeedsBundle {
			return nil, fmt.Errorf("cannot clone %s: %s", action.TargetPath, action.BundleReason)
		}
		repo := repoForTarget(manifest, action.TargetPath)
		created := false
		if isGitRepo(action.TargetPath) {
			result.Reused++
			if action.Commit != "" && !gitHasCommit(action.TargetPath, action.Commit) && action.Remote != "" {
				_ = exec.Command("git", "-C", action.TargetPath, "fetch", "--all", "--tags").Run()
			}
		} else {
			if _, err := os.Stat(action.TargetPath); err == nil {
				return nil, fmt.Errorf("target exists but is not a Git repository: %s", action.TargetPath)
			}
			if err := os.MkdirAll(filepath.Dir(action.TargetPath), 0o755); err != nil {
				return nil, err
			}
			cloneSource := action.Remote
			if cloneSource == "" {
				cloneSource = localBundlePath(opts.BundleDir, repo)
			}
			output, cloneErr := exec.Command("git", "clone", cloneSource, action.TargetPath).CombinedOutput()
			if cloneErr != nil && action.Remote != "" && repo.BundlePath != "" {
				_ = os.RemoveAll(action.TargetPath)
				bundle := localBundlePath(opts.BundleDir, repo)
				if !fileMatches(bundle, repo.BundleSHA256, repo.BundleBytes) {
					return nil, fmt.Errorf("git clone %s failed: %w: %s; rerun profile fetch with --include-git-bundles", action.Remote, cloneErr, output)
				}
				cloneSource = bundle
				output, cloneErr = exec.Command("git", "clone", cloneSource, action.TargetPath).CombinedOutput()
			}
			if cloneErr != nil {
				if repo.BundlePath != "" && !fileMatches(localBundlePath(opts.BundleDir, repo), repo.BundleSHA256, repo.BundleBytes) {
					return nil, fmt.Errorf("Git fallback is not present for %s; rerun profile fetch with --include-git-bundles", action.TargetPath)
				}
				return nil, fmt.Errorf("git clone %s: %w: %s", cloneSource, cloneErr, output)
			}
			result.Cloned++
			created = true
		}
		if created && action.Commit != "" && !gitHasCommit(action.TargetPath, action.Commit) && repo.BundlePath != "" {
			bundle := localBundlePath(opts.BundleDir, repo)
			if !fileMatches(bundle, repo.BundleSHA256, repo.BundleBytes) {
				return nil, fmt.Errorf("commit %s is not available from %s; rerun profile fetch with --include-git-bundles", action.Commit, action.Remote)
			}
			if err := os.RemoveAll(action.TargetPath); err != nil {
				return nil, err
			}
			if output, err := exec.Command("git", "clone", bundle, action.TargetPath).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git clone fallback bundle: %w: %s", err, output)
			}
		}
		if action.Commit != "" {
			if output, err := exec.Command("git", "-C", action.TargetPath, "checkout", action.Commit).CombinedOutput(); err != nil {
				return nil, fmt.Errorf("checkout %s in %s: %w: %s", action.Commit, action.TargetPath, err, output)
			}
		}
	}
	result.Status = "ok"
	return result, nil
}

func gitHasCommit(repo, commit string) bool {
	return exec.Command("git", "-C", repo, "cat-file", "-e", commit+"^{commit}").Run() == nil
}

func repoForTarget(manifest *Manifest, target string) GitRepo {
	for _, project := range manifest.Projects {
		for _, repo := range project.Repos {
			if repo.TargetPath == target {
				return repo
			}
		}
	}
	return GitRepo{}
}

func localBundlePath(bundleDir string, repo GitRepo) string {
	path, _ := safeJoin(bundleDir, repo.BundlePath)
	return path
}

func isGitRepo(path string) bool {
	command := exec.Command("git", "-C", path, "rev-parse", "--is-inside-work-tree")
	return command.Run() == nil
}
