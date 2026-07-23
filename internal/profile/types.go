package profile

import "time"

const Schema = "agent-capsule.codex-profile.v1"

var DefaultProfilePaths = []string{
	"AGENTS.md",
	"config.toml",
	"hooks.json",
	"history.jsonl",
	"rules",
	"skills",
	"memories",
	"data",
	"automations",
}

var DefaultProfileDatabases = []string{
	"memories_1.sqlite",
	"goals_1.sqlite",
}

var DefaultExclusions = []string{
	"auth.json and provider credentials",
	"installation and machine identifiers",
	"Keychain, cookies, and browser profiles",
	"plugins/cache, logs, tmp, and shell snapshots",
	"worktrees and project working trees",
	"skills/.system (managed by the target Codex installation)",
	"uncommitted and untracked project files",
}

type Manifest struct {
	Schema          string        `json:"schema"`
	ID              string        `json:"id"`
	CreatedAt       time.Time     `json:"created_at"`
	SourceHome      string        `json:"source_home"`
	TargetHome      string        `json:"target_home"`
	SourceUserHome  string        `json:"source_user_home,omitempty"`
	TargetUserHome  string        `json:"target_user_home,omitempty"`
	SourceWorkspace string        `json:"source_workspace,omitempty"`
	TargetWorkspace string        `json:"target_workspace,omitempty"`
	Projects        []Project     `json:"projects"`
	Threads         []Thread      `json:"threads"`
	ProfileFiles    []File        `json:"profile_files"`
	Exclusions      []string      `json:"exclusions"`
	Stats           ManifestStats `json:"stats"`
}

type ManifestStats struct {
	Projects     int   `json:"projects"`
	GitRepos     int   `json:"git_repos"`
	Threads      int   `json:"threads"`
	ProfileFiles int   `json:"profile_files"`
	Bytes        int64 `json:"bytes"`
}

type Project struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	SourcePath string    `json:"source_path"`
	TargetPath string    `json:"target_path"`
	Repos      []GitRepo `json:"repos"`
}

type GitRepo struct {
	RelativePath string            `json:"relative_path"`
	TargetPath   string            `json:"target_path"`
	Branch       string            `json:"branch,omitempty"`
	Commit       string            `json:"commit,omitempty"`
	Remotes      map[string]string `json:"remotes,omitempty"`
	Clean        bool              `json:"clean"`
	BundlePath   string            `json:"bundle_path,omitempty"`
	BundleSHA256 string            `json:"bundle_sha256,omitempty"`
	BundleBytes  int64             `json:"bundle_bytes,omitempty"`
}

type Thread struct {
	ID                    string         `json:"id"`
	Title                 string         `json:"title,omitempty"`
	SourceCWD             string         `json:"source_cwd"`
	TargetCWD             string         `json:"target_cwd"`
	SourceSessionPath     string         `json:"source_session_path"`
	TargetSessionRelative string         `json:"target_session_relative_path"`
	BundlePath            string         `json:"bundle_path"`
	SHA256                string         `json:"sha256"`
	Bytes                 int64          `json:"bytes"`
	Row                   map[string]any `json:"row,omitempty"`
	IndexEntry            map[string]any `json:"index_entry,omitempty"`
}

type File struct {
	TargetRelativePath string `json:"target_relative_path"`
	BundlePath         string `json:"bundle_path"`
	SHA256             string `json:"sha256"`
	Bytes              int64  `json:"bytes"`
	Mode               uint32 `json:"mode"`
	LinkTarget         string `json:"link_target,omitempty"`
}

type ExportOptions struct {
	Home                 string
	TargetHome           string
	TargetWorkspace      string
	Projects             []string
	Out                  string
	UnsafeIncludeSecrets bool
	GitBundleFallback    bool
}

type ExportResult struct {
	Status       string        `json:"status"`
	BundleDir    string        `json:"bundle_dir"`
	ManifestPath string        `json:"manifest_path"`
	Manifest     *Manifest     `json:"manifest"`
	ClonePlan    []CloneAction `json:"clone_plan"`
}

type CloneAction struct {
	Project      string `json:"project"`
	TargetPath   string `json:"target_path"`
	Remote       string `json:"remote,omitempty"`
	Branch       string `json:"branch,omitempty"`
	Commit       string `json:"commit,omitempty"`
	Command      string `json:"command,omitempty"`
	NeedsBundle  bool   `json:"needs_bundle"`
	BundleReason string `json:"bundle_reason,omitempty"`
}

type FetchOptions struct {
	Source            string
	Out               string
	IncludeGitBundles bool
}

type FetchResult struct {
	Status          string `json:"status"`
	BundleDir       string `json:"bundle_dir"`
	DownloadedFiles int    `json:"downloaded_files"`
	ReusedFiles     int    `json:"reused_files"`
	DownloadedBytes int64  `json:"downloaded_bytes"`
}

type ImportOptions struct {
	BundleDir      string
	Home           string
	Execute        bool
	RequireStopped bool
}

type ImportResult struct {
	Status            string   `json:"status"`
	DryRun            bool     `json:"dry_run"`
	TargetHome        string   `json:"target_home"`
	BackupDir         string   `json:"backup_dir,omitempty"`
	Projects          int      `json:"projects"`
	Threads           int      `json:"threads"`
	ProfileFiles      int      `json:"profile_files"`
	MissingProjects   []string `json:"missing_projects,omitempty"`
	Writes            []string `json:"writes"`
	PreservedIdentity bool     `json:"preserved_target_identity"`
}

type VerifyOptions struct {
	BundleDir string
	Home      string
}

type VerifyResult struct {
	Status        string   `json:"status"`
	TargetHome    string   `json:"target_home"`
	Projects      int      `json:"projects"`
	Threads       int      `json:"threads"`
	ProfileFiles  int      `json:"profile_files"`
	DatabaseCheck string   `json:"database_check,omitempty"`
	Failures      []string `json:"failures,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type ScheduleOptions struct {
	BundleDir string
	Home      string
	CLIPath   string
	Submit    bool
}

type ScheduleResult struct {
	Status     string `json:"status"`
	Label      string `json:"label"`
	StagingDir string `json:"staging_dir"`
	PlistPath  string `json:"plist_path"`
	RunnerPath string `json:"runner_path"`
	StatusPath string `json:"status_path"`
	LogPath    string `json:"log_path"`
}

type UnscheduleOptions struct {
	BundleDir string
	Home      string
	Submit    bool
}

type UnscheduleResult struct {
	Status    string `json:"status"`
	Label     string `json:"label"`
	PlistPath string `json:"plist_path"`
}
