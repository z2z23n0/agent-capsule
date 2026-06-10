package capsule

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/z2z23n0/agent-capsule/internal/codex"
)

const (
	SchemaVersion = 1
	ArtifactType  = "agent-capsule"
	DefaultRepo   = "https://github.com/z2z23n0/agent-capsule"
	InstallCmd    = "go install github.com/z2z23n0/agent-capsule/cmd/capsule@main"
)

var RequiredFiles = []string{
	"manifest.json",
	"AGENT_README.md",
	"codex/session.jsonl",
	"codex/index-entry.json",
	"codex/thread-row.json",
	"agent/restore.md",
	"safety/scan.json",
	"checksums.json",
}

type Manifest struct {
	SchemaVersion             int               `json:"schema_version"`
	ArtifactType              string            `json:"artifact_type"`
	CreatedAt                 string            `json:"created_at"`
	ThreadID                  string            `json:"thread_id"`
	ThreadTitle               string            `json:"thread_title"`
	SourceHome                string            `json:"source_home"`
	SourceCWD                 string            `json:"source_cwd"`
	SourceSessionRelativePath string            `json:"source_session_relative_path"`
	SourceCLIVersion          string            `json:"source_cli_version,omitempty"`
	SourceModelProvider       string            `json:"source_model_provider,omitempty"`
	RepoURL                   string            `json:"repo_url"`
	SkillURL                  string            `json:"skill_url"`
	InstallCommand            string            `json:"install_command"`
	RestoreCommand            string            `json:"restore_command"`
	Git                       map[string]string `json:"git,omitempty"`
	Files                     []string          `json:"files"`
	Notes                     []string          `json:"notes"`
}

type ChecksumFile struct {
	Algorithm string            `json:"algorithm"`
	Files     map[string]string `json:"files"`
}

type SafetyScan struct {
	Status   string          `json:"status"`
	Findings []SafetyFinding `json:"findings"`
}

type SafetyFinding struct {
	Rule    string `json:"rule"`
	Line    int    `json:"line"`
	Excerpt string `json:"excerpt"`
}

type ExportOptions struct {
	Home                 string
	Thread               string
	Out                  string
	UnsafeIncludeSecrets bool
}

type ExportResult struct {
	Status    string     `json:"status"`
	Path      string     `json:"path"`
	ThreadID  string     `json:"thread_id"`
	Title     string     `json:"title"`
	Safety    SafetyScan `json:"safety"`
	Bytes     int64      `json:"bytes"`
	Checksums string     `json:"checksums"`
}

type InspectResult struct {
	Status    string     `json:"status"`
	Path      string     `json:"path"`
	Manifest  Manifest   `json:"manifest"`
	Safety    SafetyScan `json:"safety"`
	Checksums string     `json:"checksums"`
}

type loadedCapsule struct {
	Manifest    Manifest
	Session     []byte
	IndexEntry  map[string]any
	ThreadRow   map[string]any
	Safety      SafetyScan
	Checksums   ChecksumFile
	RawPayloads map[string][]byte
}

func Export(opts ExportOptions) (*ExportResult, error) {
	home, err := codex.ResolveHome(opts.Home)
	if err != nil {
		return nil, err
	}
	threadID, err := codex.ResolveThreadID(home, opts.Thread)
	if err != nil {
		return nil, err
	}
	data, err := codex.ExportThread(home, threadID)
	if err != nil {
		return nil, err
	}
	scan := ScanSecrets(data.SessionBytes)
	if len(scan.Findings) > 0 && !opts.UnsafeIncludeSecrets {
		return nil, fmt.Errorf("secret scan found %d finding(s); re-run with --unsafe-include-secrets only if this is intentional", len(scan.Findings))
	}
	out := opts.Out
	if out == "" {
		out = DefaultOutputName(data.Title, data.ThreadID)
	}
	manifest := buildManifest(data)
	files, err := buildFiles(manifest, data, scan)
	if err != nil {
		return nil, err
	}
	checksums := buildChecksums(files)
	checksumPayload, err := jsonBytes(checksums)
	if err != nil {
		return nil, err
	}
	files["checksums.json"] = checksumPayload
	if err := writeZip(out, files); err != nil {
		return nil, err
	}
	info, _ := os.Stat(out)
	return &ExportResult{
		Status:    "ok",
		Path:      out,
		ThreadID:  data.ThreadID,
		Title:     data.Title,
		Safety:    scan,
		Bytes:     fileSize(info),
		Checksums: "sha256",
	}, nil
}

func Inspect(path string) (*InspectResult, error) {
	loaded, err := load(path)
	if err != nil {
		return nil, err
	}
	return &InspectResult{
		Status:    "ok",
		Path:      path,
		Manifest:  loaded.Manifest,
		Safety:    loaded.Safety,
		Checksums: loaded.Checksums.Algorithm,
	}, nil
}

func Restore(path string, opts codex.RestoreOptions) (*codex.RestoreResult, error) {
	loaded, err := load(path)
	if err != nil {
		return nil, err
	}
	input := codex.RestoreInput{
		ThreadID:                  loaded.Manifest.ThreadID,
		Title:                     loaded.Manifest.ThreadTitle,
		SourceSessionRelativePath: loaded.Manifest.SourceSessionRelativePath,
		SessionBytes:              loaded.Session,
		IndexEntry:                loaded.IndexEntry,
		ThreadRow:                 loaded.ThreadRow,
	}
	return codex.RestoreThread(input, opts)
}

func Verify(home, threadID, expectedCWD string) (*codex.VerifyResult, error) {
	return codex.VerifyThread(home, threadID, expectedCWD)
}

func DefaultOutputName(title, threadID string) string {
	base := sanitizeFilename(title)
	if base == "" {
		base = threadID
	}
	if base == "" {
		base = "session"
	}
	return base + ".capsule.zip"
}

func ScanSecrets(content []byte) SafetyScan {
	rules := map[string]*regexp.Regexp{
		"openai_api_key": regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
		"github_token":   regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{30,}`),
		"aws_access_key": regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		"slack_token":    regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{20,}`),
		"named_secret":   regexp.MustCompile(`(?i)(api[_-]?key|access[_-]?token|refresh[_-]?token|secret|password)\s*[:=]\s*["'][^"']{16,}["']`),
	}
	var findings []SafetyFinding
	lines := strings.Split(string(content), "\n")
	for i, line := range lines {
		for name, pattern := range rules {
			if pattern.FindStringIndex(line) != nil {
				findings = append(findings, SafetyFinding{
					Rule:    name,
					Line:    i + 1,
					Excerpt: redactLine(line),
				})
			}
		}
	}
	status := "ok"
	if len(findings) > 0 {
		status = "blocked"
	}
	return SafetyScan{Status: status, Findings: findings}
}

func buildManifest(data *codex.ExportData) Manifest {
	return Manifest{
		SchemaVersion:             SchemaVersion,
		ArtifactType:              ArtifactType,
		CreatedAt:                 time.Now().UTC().Format(time.RFC3339Nano),
		ThreadID:                  data.ThreadID,
		ThreadTitle:               data.Title,
		SourceHome:                data.SourceHome,
		SourceCWD:                 data.SourceCWD,
		SourceSessionRelativePath: data.SourceSessionRelativePath,
		SourceCLIVersion:          data.Summary.CLIVersion,
		SourceModelProvider:       data.Summary.ModelProvider,
		RepoURL:                   DefaultRepo,
		SkillURL:                  DefaultRepo,
		InstallCommand:            InstallCmd,
		RestoreCommand:            "capsule restore <this-file>.capsule.zip --target codex --target-cwd . --execute",
		Git:                       gitMetadata(data.ThreadRow),
		Files:                     RequiredFiles,
		Notes: []string{
			"v0.1 restores Codex UI visibility from a local zip file.",
			"It does not migrate auth, provider credentials, cloud state, or guarantee encrypted_content can continue cryptographically unchanged.",
		},
	}
}

func buildFiles(manifest Manifest, data *codex.ExportData, scan SafetyScan) (map[string][]byte, error) {
	manifestPayload, err := jsonBytes(manifest)
	if err != nil {
		return nil, err
	}
	indexPayload, err := jsonBytes(data.IndexEntry)
	if err != nil {
		return nil, err
	}
	threadPayload, err := jsonBytes(data.ThreadRow)
	if err != nil {
		return nil, err
	}
	scanPayload, err := jsonBytes(scan)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"manifest.json":          manifestPayload,
		"AGENT_README.md":        []byte(agentReadme(manifest)),
		"codex/session.jsonl":    data.SessionBytes,
		"codex/index-entry.json": indexPayload,
		"codex/thread-row.json":  threadPayload,
		"agent/restore.md":       []byte(restoreMarkdown(manifest, data.Summary)),
		"safety/scan.json":       scanPayload,
	}, nil
}

func buildChecksums(files map[string][]byte) ChecksumFile {
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	checksums := ChecksumFile{Algorithm: "sha256", Files: map[string]string{}}
	for _, name := range names {
		digest := sha256.Sum256(files[name])
		checksums.Files[name] = hex.EncodeToString(digest[:])
	}
	return checksums
}

func writeZip(path string, files map[string][]byte) error {
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(path)), 0o755); err != nil && filepath.Dir(filepath.Clean(path)) != "." {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	archive := zip.NewWriter(file)
	defer archive.Close()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetModTime(time.Unix(0, 0).UTC())
		writer, err := archive.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := writer.Write(files[name]); err != nil {
			return err
		}
	}
	return nil
}

func load(path string) (*loadedCapsule, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	payloads := map[string][]byte{}
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		content, readErr := io.ReadAll(rc)
		closeErr := rc.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		payloads[file.Name] = content
	}
	for _, name := range RequiredFiles {
		if _, ok := payloads[name]; !ok {
			return nil, fmt.Errorf("capsule missing required file %s", name)
		}
	}
	var checksums ChecksumFile
	if err := json.Unmarshal(payloads["checksums.json"], &checksums); err != nil {
		return nil, err
	}
	if err := verifyChecksums(payloads, checksums); err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(payloads["manifest.json"], &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != SchemaVersion || manifest.ArtifactType != ArtifactType {
		return nil, fmt.Errorf("unsupported capsule schema/type: %d %s", manifest.SchemaVersion, manifest.ArtifactType)
	}
	var indexEntry map[string]any
	if err := json.Unmarshal(payloads["codex/index-entry.json"], &indexEntry); err != nil {
		return nil, err
	}
	var threadRow map[string]any
	if err := json.Unmarshal(payloads["codex/thread-row.json"], &threadRow); err != nil {
		return nil, err
	}
	var scan SafetyScan
	if err := json.Unmarshal(payloads["safety/scan.json"], &scan); err != nil {
		return nil, err
	}
	return &loadedCapsule{
		Manifest:    manifest,
		Session:     payloads["codex/session.jsonl"],
		IndexEntry:  indexEntry,
		ThreadRow:   threadRow,
		Safety:      scan,
		Checksums:   checksums,
		RawPayloads: payloads,
	}, nil
}

func verifyChecksums(payloads map[string][]byte, checksums ChecksumFile) error {
	if checksums.Algorithm != "sha256" {
		return fmt.Errorf("unsupported checksum algorithm %s", checksums.Algorithm)
	}
	for name, expected := range checksums.Files {
		payload, ok := payloads[name]
		if !ok {
			return fmt.Errorf("checksum references missing file %s", name)
		}
		digest := sha256.Sum256(payload)
		if hex.EncodeToString(digest[:]) != expected {
			return fmt.Errorf("checksum mismatch for %s", name)
		}
	}
	return nil
}

func agentReadme(manifest Manifest) string {
	return fmt.Sprintf(`# Agent Capsule

This is a standard zip file for restoring one Codex session into a local Codex UI.

Start here:

1. Read manifest.json and safety/scan.json.
2. Install the CLI if it is missing:

   %s

3. Inspect the capsule:

   capsule inspect <this-file>.capsule.zip

4. Restore into the current project when the user approves local Codex history writes:

   %s

5. Read agent/restore.md for the continuation context.

Important boundaries:

- Do not migrate auth, provider credentials, or secrets.
- capsule restore is dry-run by default; --execute is required to write.
- If the target already has this thread id, restore refuses unless --replace is provided.

Thread: %s
Title: %s
`, manifest.InstallCommand, manifest.RestoreCommand, manifest.ThreadID, manifest.ThreadTitle)
}

func restoreMarkdown(manifest Manifest, summary codex.SessionSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Restore Context\n\n")
	fmt.Fprintf(&b, "- Thread ID: `%s`\n", manifest.ThreadID)
	fmt.Fprintf(&b, "- Title: %s\n", manifest.ThreadTitle)
	fmt.Fprintf(&b, "- Source cwd: `%s`\n", manifest.SourceCWD)
	if manifest.SourceCLIVersion != "" {
		fmt.Fprintf(&b, "- Source Codex CLI: `%s`\n", manifest.SourceCLIVersion)
	}
	fmt.Fprintf(&b, "\n## Continue\n\n")
	fmt.Fprintf(&b, "After restoring, open the imported Codex thread in the local UI. Treat this file as a compact handoff if encrypted or private runtime state cannot be continued exactly.\n\n")
	if summary.FirstUserText != "" {
		fmt.Fprintf(&b, "## First User Message\n\n%s\n\n", summary.FirstUserText)
	}
	if summary.LastAgentText != "" {
		fmt.Fprintf(&b, "## Last Agent Message\n\n%s\n\n", summary.LastAgentText)
	}
	if len(summary.VisibleExcerpt) > 0 {
		fmt.Fprintf(&b, "## Visible Excerpt\n\n")
		for _, item := range summary.VisibleExcerpt {
			fmt.Fprintf(&b, "- %s\n", item)
		}
		fmt.Fprintf(&b, "\n")
	}
	fmt.Fprintf(&b, "## Recovery Command\n\n```bash\n%s\n```\n", manifest.RestoreCommand)
	return b.String()
}

func gitMetadata(row map[string]any) map[string]string {
	git := map[string]string{}
	for _, key := range []string{"git_sha", "git_branch", "git_origin_url"} {
		if value, ok := row[key].(string); ok && value != "" {
			git[key] = value
		}
	}
	if len(git) == 0 {
		return nil
	}
	return git
}

func jsonBytes(value any) ([]byte, error) {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(payload, '\n'), nil
}

func sanitizeFilename(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\n", " ")
	replacer := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "*", "-", "?", "-", "\"", "'", "<", "-", ">", "-", "|", "-")
	value = replacer.Replace(value)
	value = strings.Join(strings.Fields(value), "-")
	value = strings.Trim(value, ".-")
	if len(value) > 80 {
		value = value[:80]
	}
	return value
}

func redactLine(line string) string {
	line = strings.TrimSpace(line)
	if len(line) > 160 {
		line = line[:160] + "..."
	}
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`sk-[A-Za-z0-9_-]{20,}`),
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]{30,}`),
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{20,}`),
	} {
		line = pattern.ReplaceAllString(line, "[REDACTED]")
	}
	return line
}

func fileSize(info os.FileInfo) int64 {
	if info == nil {
		return 0
	}
	return info.Size()
}

func IsNotCapsule(err error) bool {
	return errors.Is(err, zip.ErrFormat)
}
