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

	"github.com/z2z23n0/agent-capsule/internal/claude"
	"github.com/z2z23n0/agent-capsule/internal/codex"
	"github.com/z2z23n0/agent-capsule/internal/neutral"
)

const (
	SchemaVersion = 1
	ArtifactType  = "agent-capsule"
	AgentCodex    = "codex"
	AgentClaude   = "claude"
	DefaultRepo   = "https://github.com/z2z23n0/agent-capsule"
	DefaultSkill  = DefaultRepo + "/tree/main/skills/agent-capsule"
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

var ClaudeRequiredFiles = []string{
	"manifest.json",
	"AGENT_README.md",
	"claude/session.jsonl",
	"claude/session-index-entry.json",
	"agent/neutral.json",
	"agent/restore.md",
	"safety/scan.json",
	"checksums.json",
}

type Manifest struct {
	SchemaVersion             int               `json:"schema_version"`
	ArtifactType              string            `json:"artifact_type"`
	SourceAgent               string            `json:"source_agent,omitempty"`
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
	TargetSupport             []string          `json:"target_support,omitempty"`
	Payloads                  []Payload         `json:"payloads,omitempty"`
	LosslessLevel             string            `json:"lossless_level,omitempty"`
	Notes                     []string          `json:"notes"`
}

type Payload struct {
	Agent       string `json:"agent"`
	Kind        string `json:"kind"`
	Path        string `json:"path"`
	Lossless    string `json:"lossless,omitempty"`
	Description string `json:"description,omitempty"`
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
	SourceAgent          string
	Home                 string
	Thread               string
	Out                  string
	Name                 string
	UnsafeIncludeSecrets bool
}

type ExportResult struct {
	Status    string       `json:"status"`
	Source    string       `json:"source_agent"`
	Path      string       `json:"path"`
	ThreadID  string       `json:"thread_id"`
	Title     string       `json:"title"`
	Safety    SafetyScan   `json:"safety"`
	Images    ImageSummary `json:"images"`
	Bytes     int64        `json:"bytes"`
	Checksums string       `json:"checksums"`
}

type InspectResult struct {
	Status    string       `json:"status"`
	Path      string       `json:"path"`
	Manifest  Manifest     `json:"manifest"`
	Safety    SafetyScan   `json:"safety"`
	Images    ImageSummary `json:"images"`
	Checksums string       `json:"checksums"`
}

type loadedCapsule struct {
	Manifest         Manifest
	Session          []byte
	IndexEntry       map[string]any
	ThreadRow        map[string]any
	Safety           SafetyScan
	Images           ImageSummary
	ImageAssets      []imageAssetFile
	ClaudeSession    []byte
	ClaudeIndexEntry map[string]any
	Neutral          neutral.Transcript
	Checksums        ChecksumFile
	RawPayloads      map[string][]byte
}

func Export(opts ExportOptions) (*ExportResult, error) {
	source := normalizeAgent(opts.SourceAgent, AgentCodex)
	switch source {
	case AgentCodex:
		return exportCodex(opts)
	case AgentClaude:
		return exportClaude(opts)
	default:
		return nil, fmt.Errorf("unsupported source agent %q", opts.SourceAgent)
	}
}

func exportCodex(opts ExportOptions) (*ExportResult, error) {
	home, err := codex.ResolveHome(opts.Home)
	if err != nil {
		return nil, err
	}
	threadID, err := codex.ResolveThreadID(home, opts.Thread)
	if err != nil {
		return nil, err
	}
	data, err := codex.ExportThreadWithOptions(home, threadID, codex.ExportThreadOptions{
		DropSelfExportTurn: shouldDropSelfExportTurn(opts.Thread, threadID),
	})
	if err != nil {
		return nil, err
	}
	scan := ScanSecrets(data.SessionBytes)
	if len(scan.Findings) > 0 && !opts.UnsafeIncludeSecrets {
		return nil, fmt.Errorf("secret scan found %d finding(s); re-run with --unsafe-include-secrets only if this is intentional", len(scan.Findings))
	}
	out := opts.Out
	if out == "" {
		out = DefaultOutputName(opts.Name, data.Title, data.Summary.FirstUserText, data.ThreadID)
	}
	imageBundle := buildImageBundle(data)
	transcript := neutral.FromCodexSession(data.ThreadID, data.Title, data.SourceCWD, data.SessionBytes)
	manifest := buildCodexManifest(data)
	addImageFilesToManifest(&manifest, imageBundle)
	files, err := buildCodexFiles(manifest, data, scan, imageBundle, transcript)
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
		Source:    AgentCodex,
		Path:      out,
		ThreadID:  data.ThreadID,
		Title:     data.Title,
		Safety:    scan,
		Images:    imageBundle.summary(),
		Bytes:     fileSize(info),
		Checksums: "sha256",
	}, nil
}

func exportClaude(opts ExportOptions) (*ExportResult, error) {
	data, err := claude.ExportSession(claude.ExportOptions{Home: opts.Home, Session: opts.Thread})
	if err != nil {
		return nil, err
	}
	scan := ScanSecrets(data.SessionBytes)
	if len(scan.Findings) > 0 && !opts.UnsafeIncludeSecrets {
		return nil, fmt.Errorf("secret scan found %d finding(s); re-run with --unsafe-include-secrets only if this is intentional", len(scan.Findings))
	}
	out := opts.Out
	if out == "" {
		out = DefaultOutputName(opts.Name, data.Title, data.Summary.FirstUserText, data.SessionID)
	}
	manifest := buildClaudeManifest(data)
	files, err := buildClaudeFiles(manifest, data, scan)
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
		Source:    AgentClaude,
		Path:      out,
		ThreadID:  data.SessionID,
		Title:     data.Title,
		Safety:    scan,
		Bytes:     fileSize(info),
		Checksums: "sha256",
	}, nil
}

func shouldDropSelfExportTurn(requested, resolved string) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" || requested == "current" {
		return true
	}
	for _, key := range []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		if os.Getenv(key) == resolved && requested == resolved {
			return true
		}
	}
	return false
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
		Images:    loaded.Images,
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
		ImageAssets:               restoreImageAssets(loaded.ImageAssets),
	}
	return codex.RestoreThread(input, opts)
}

func Verify(home, threadID, expectedCWD string) (*codex.VerifyResult, error) {
	return codex.VerifyThread(home, threadID, expectedCWD)
}

func DefaultOutputName(name, title, firstUserText, threadID string) string {
	if strings.TrimSpace(title) == threadID {
		title = ""
	}
	for _, candidate := range []string{name, title, firstUserText, threadID, "session"} {
		base := sanitizeFilename(candidate)
		if base == "" {
			continue
		}
		return capsuleFilename(base)
	}
	return "session.capsule.zip"
}

func capsuleFilename(base string) string {
	if strings.HasSuffix(base, ".capsule.zip") {
		return base
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

func buildCodexManifest(data *codex.ExportData) Manifest {
	return Manifest{
		SchemaVersion:             SchemaVersion,
		ArtifactType:              ArtifactType,
		SourceAgent:               AgentCodex,
		CreatedAt:                 time.Now().UTC().Format(time.RFC3339Nano),
		ThreadID:                  data.ThreadID,
		ThreadTitle:               data.Title,
		SourceHome:                data.SourceHome,
		SourceCWD:                 data.SourceCWD,
		SourceSessionRelativePath: data.SourceSessionRelativePath,
		SourceCLIVersion:          data.Summary.CLIVersion,
		SourceModelProvider:       data.Summary.ModelProvider,
		RepoURL:                   DefaultRepo,
		SkillURL:                  DefaultSkill,
		InstallCommand:            InstallCmd,
		RestoreCommand:            "capsule import <this-file>.capsule.zip --target codex --target-cwd . --execute",
		Git:                       gitMetadata(data.ThreadRow),
		Files:                     append(append([]string(nil), RequiredFiles...), "agent/neutral.json"),
		TargetSupport:             []string{AgentCodex, AgentClaude},
		Payloads: []Payload{
			{Agent: AgentCodex, Kind: "native-session", Path: "codex/session.jsonl", Lossless: "raw"},
			{Agent: "neutral", Kind: "transcript", Path: "agent/neutral.json", Lossless: "semantic"},
		},
		LosslessLevel: "same-agent-raw cross-agent-semantic",
		Notes: []string{
			"Imports a Codex session from a local zip file as a new thread.",
			"Cross-agent imports preserve visible messages, tool evidence, and raw source sidecars.",
			"It does not migrate auth, provider credentials, cloud state, or guarantee encrypted_content can continue cryptographically unchanged.",
		},
	}
}

func addImageFilesToManifest(manifest *Manifest, bundle imageBundle) {
	if !bundle.hasManifest() {
		return
	}
	manifest.Files = append(manifest.Files, imageFiles(bundle)...)
	manifest.Notes = append(manifest.Notes, "Image assets may include user-uploaded local image files referenced by this Codex session.")
}

func buildClaudeManifest(data *claude.ExportData) Manifest {
	files := append([]string(nil), ClaudeRequiredFiles...)
	return Manifest{
		SchemaVersion:             SchemaVersion,
		ArtifactType:              ArtifactType,
		SourceAgent:               AgentClaude,
		CreatedAt:                 time.Now().UTC().Format(time.RFC3339Nano),
		ThreadID:                  data.SessionID,
		ThreadTitle:               data.Title,
		SourceHome:                data.SourceHome,
		SourceCWD:                 data.SourceCWD,
		SourceSessionRelativePath: data.SourceSessionRelativePath,
		SourceCLIVersion:          "claude-code",
		SourceModelProvider:       "anthropic",
		RepoURL:                   DefaultRepo,
		SkillURL:                  DefaultSkill,
		InstallCommand:            InstallCmd,
		RestoreCommand:            "capsule import <this-file>.capsule.zip --target claude --target-cwd . --execute",
		Git:                       claudeGitMetadata(data.Summary),
		Files:                     files,
		TargetSupport:             []string{AgentClaude, AgentCodex},
		Payloads: []Payload{
			{Agent: AgentClaude, Kind: "native-session", Path: "claude/session.jsonl", Lossless: "raw"},
			{Agent: "neutral", Kind: "transcript", Path: "agent/neutral.json", Lossless: "semantic"},
		},
		LosslessLevel: "same-agent-raw cross-agent-semantic",
		Notes: []string{
			"Imports a Claude Code session into local Claude Code history as a new session.",
			"Cross-agent imports preserve visible messages, tool evidence, and raw source sidecars.",
			"It does not migrate auth, provider credentials, cloud state, or filesystem checkpoint state.",
		},
	}
}

func buildCodexFiles(manifest Manifest, data *codex.ExportData, scan SafetyScan, imageBundle imageBundle, transcript neutral.Transcript) (map[string][]byte, error) {
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
	neutralPayload, err := jsonBytes(transcript)
	if err != nil {
		return nil, err
	}
	files := map[string][]byte{
		"manifest.json":          manifestPayload,
		"AGENT_README.md":        []byte(agentReadme(manifest)),
		"codex/session.jsonl":    data.SessionBytes,
		"codex/index-entry.json": indexPayload,
		"codex/thread-row.json":  threadPayload,
		"agent/neutral.json":     neutralPayload,
		"agent/restore.md":       []byte(restoreMarkdown(manifest, data.Summary)),
		"safety/scan.json":       scanPayload,
	}
	if imageBundle.hasManifest() {
		imagePayload, err := jsonBytes(imageBundle.Manifest)
		if err != nil {
			return nil, err
		}
		files[ImageAssetsManifestPath] = imagePayload
		for name, content := range imageBundle.Files {
			files[name] = content
		}
	}
	return files, nil
}

func buildClaudeFiles(manifest Manifest, data *claude.ExportData, scan SafetyScan) (map[string][]byte, error) {
	manifestPayload, err := jsonBytes(manifest)
	if err != nil {
		return nil, err
	}
	indexPayload, err := jsonBytes(data.IndexEntry)
	if err != nil {
		return nil, err
	}
	neutralPayload, err := jsonBytes(data.Neutral)
	if err != nil {
		return nil, err
	}
	scanPayload, err := jsonBytes(scan)
	if err != nil {
		return nil, err
	}
	return map[string][]byte{
		"manifest.json":                   manifestPayload,
		"AGENT_README.md":                 []byte(agentReadme(manifest)),
		"claude/session.jsonl":            data.SessionBytes,
		"claude/session-index-entry.json": indexPayload,
		"agent/neutral.json":              neutralPayload,
		"agent/restore.md":                []byte(restoreMarkdownFromNeutral(manifest, data.Neutral)),
		"safety/scan.json":                scanPayload,
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
	sourceAgent := normalizeAgent(manifest.SourceAgent, AgentCodex)
	var scan SafetyScan
	if err := json.Unmarshal(payloads["safety/scan.json"], &scan); err != nil {
		return nil, err
	}
	loaded := &loadedCapsule{
		Manifest:    manifest,
		Safety:      scan,
		Checksums:   checksums,
		RawPayloads: payloads,
	}
	if payload, ok := payloads["agent/neutral.json"]; ok {
		_ = json.Unmarshal(payload, &loaded.Neutral)
	}
	switch sourceAgent {
	case AgentCodex:
		for _, name := range RequiredFiles {
			if _, ok := payloads[name]; !ok {
				return nil, fmt.Errorf("capsule missing required file %s", name)
			}
		}
		var indexEntry map[string]any
		if err := json.Unmarshal(payloads["codex/index-entry.json"], &indexEntry); err != nil {
			return nil, err
		}
		var threadRow map[string]any
		if err := json.Unmarshal(payloads["codex/thread-row.json"], &threadRow); err != nil {
			return nil, err
		}
		imageAssets, imageSummary, err := loadImageAssets(payloads)
		if err != nil {
			return nil, err
		}
		imageSummary = summarizeLoadedImages(payloads["codex/session.jsonl"], imageSummary)
		loaded.Session = payloads["codex/session.jsonl"]
		loaded.IndexEntry = indexEntry
		loaded.ThreadRow = threadRow
		loaded.Images = imageSummary
		loaded.ImageAssets = imageAssets
		if loaded.Neutral.Schema == "" {
			loaded.Neutral = neutral.FromCodexSession(manifest.ThreadID, manifest.ThreadTitle, manifest.SourceCWD, loaded.Session)
		}
	case AgentClaude:
		for _, name := range ClaudeRequiredFiles {
			if _, ok := payloads[name]; !ok {
				return nil, fmt.Errorf("capsule missing required file %s", name)
			}
		}
		var indexEntry map[string]any
		if err := json.Unmarshal(payloads["claude/session-index-entry.json"], &indexEntry); err != nil {
			return nil, err
		}
		loaded.ClaudeSession = payloads["claude/session.jsonl"]
		loaded.ClaudeIndexEntry = indexEntry
		if loaded.Neutral.Schema == "" {
			loaded.Neutral = neutral.FromClaudeSession(manifest.ThreadID, manifest.ThreadTitle, manifest.SourceCWD, loaded.ClaudeSession)
		}
	default:
		return nil, fmt.Errorf("unsupported capsule source agent %q", sourceAgent)
	}
	return loaded, nil
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
	source := normalizeAgent(manifest.SourceAgent, AgentCodex)
	return fmt.Sprintf(`# Agent Capsule

This is a standard zip file for importing one %s session into a local coding agent history as a new session/thread.

Start here:

1. Read manifest.json, safety/scan.json, and any agent-specific asset manifests if present.
2. Install the CLI if it is missing:

   %s

3. Inspect the capsule:

   capsule inspect <this-file>.capsule.zip

4. Optionally read the Agent Capsule skill for a richer agent workflow:

   %s

5. Import into the current project when the user approves local agent history writes:

   %s

6. Read agent/restore.md for the continuation context.

Important boundaries:

- Do not migrate auth, provider credentials, or secrets.
- Only import after the user approves local agent history writes; the recovery command above includes the required --execute flag.
- Import always creates a new thread id, like a Codex session fork.
- Cross-agent imports preserve visible messages, tool evidence, and raw source sidecars; they do not replay private runtime state.
- This capsule may include image assets referenced by the source session; inspect image counts before importing.

Source agent: %s
Session/thread: %s
Title: %s
`, source, manifest.InstallCommand, manifest.SkillURL, manifest.RestoreCommand, source, manifest.ThreadID, manifest.ThreadTitle)
}

func restoreMarkdown(manifest Manifest, summary codex.SessionSummary) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Restore Context\n\n")
	fmt.Fprintf(&b, "- Source agent: `%s`\n", normalizeAgent(manifest.SourceAgent, AgentCodex))
	fmt.Fprintf(&b, "- Source thread ID: `%s`\n", manifest.ThreadID)
	fmt.Fprintf(&b, "- Title: %s\n", manifest.ThreadTitle)
	fmt.Fprintf(&b, "- Source cwd: `%s`\n", manifest.SourceCWD)
	if manifest.SourceCLIVersion != "" {
		fmt.Fprintf(&b, "- Source Codex CLI: `%s`\n", manifest.SourceCLIVersion)
	}
	fmt.Fprintf(&b, "\n## Continue\n\n")
	fmt.Fprintf(&b, "After importing, open the new Codex thread in the local UI. Treat this file as a compact handoff if encrypted or private runtime state cannot be continued exactly.\n\n")
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

func restoreMarkdownFromNeutral(manifest Manifest, transcript neutral.Transcript) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Restore Context\n\n")
	fmt.Fprintf(&b, "- Source agent: `%s`\n", normalizeAgent(manifest.SourceAgent, transcript.SourceAgent))
	fmt.Fprintf(&b, "- Source session/thread ID: `%s`\n", manifest.ThreadID)
	fmt.Fprintf(&b, "- Title: %s\n", manifest.ThreadTitle)
	fmt.Fprintf(&b, "- Source cwd: `%s`\n", manifest.SourceCWD)
	fmt.Fprintf(&b, "- Lossless level: `%s`\n", manifest.LosslessLevel)
	fmt.Fprintf(&b, "\n## Continue\n\n")
	fmt.Fprintf(&b, "After importing, open the new target-agent session. Same-agent imports preserve the raw transcript with a new id; cross-agent imports preserve visible messages, tool evidence, and a raw source sidecar.\n\n")
	fmt.Fprintf(&b, "## Handoff Transcript\n\n%s\n\n", neutral.HandoffText(transcript, "target agent"))
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

func claudeGitMetadata(summary claude.SessionSummary) map[string]string {
	if summary.GitBranch == "" {
		return nil
	}
	return map[string]string{"git_branch": summary.GitBranch}
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
	runes := []rune(value)
	if len(runes) > 80 {
		value = string(runes[:80])
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
