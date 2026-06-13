package claude

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/z2z23n0/agent-capsule/internal/neutral"
)

const (
	ProjectsDir = "projects"
	IndexFile   = "sessions-index.json"
)

type ExportData struct {
	SessionID                 string
	Title                     string
	SourceHome                string
	SourceCWD                 string
	SourceSessionRelativePath string
	SessionBytes              []byte
	IndexEntry                map[string]any
	Neutral                   neutral.Transcript
	Summary                   SessionSummary
}

type SessionSummary struct {
	ID            string
	CWD           string
	FirstUserText string
	LastAgentText string
	CreatedAt     string
	ModifiedAt    string
	GitBranch     string
}

type ExportOptions struct {
	Home    string
	Session string
}

type RestoreInput struct {
	SourceAgent               string
	SessionID                 string
	Title                     string
	SourceSessionRelativePath string
	SessionBytes              []byte
	Neutral                   neutral.Transcript
	RawSidecar                []byte
	ManifestSidecar           []byte
}

type RestoreOptions struct {
	Home      string
	TargetCWD string
	Execute   bool
}

type RestoreResult struct {
	SourceAgent       string   `json:"source_agent"`
	SourceSessionID   string   `json:"source_session_id"`
	Status            string   `json:"status"`
	Error             string   `json:"error,omitempty"`
	SessionID         string   `json:"session_id"`
	TargetHome        string   `json:"target_home"`
	TargetCWD         string   `json:"target_cwd"`
	TargetSessionPath string   `json:"target_session_path"`
	SidecarDir        string   `json:"sidecar_dir,omitempty"`
	BackupDir         string   `json:"backup_dir,omitempty"`
	Writes            []string `json:"writes"`
	DryRun            bool     `json:"dry_run"`
	FallbackCommand   string   `json:"fallback_command,omitempty"`
}

type VerifyResult struct {
	Status            string   `json:"status"`
	SessionID         string   `json:"session_id"`
	TargetHome        string   `json:"target_home"`
	ExpectedCWD       string   `json:"expected_cwd,omitempty"`
	SessionFileExists bool     `json:"session_file_exists"`
	IndexEntryExists  bool     `json:"index_entry_exists"`
	SessionCWDs       []string `json:"session_cwds"`
	Failures          []string `json:"failures,omitempty"`
}

type projectIndex struct {
	Version      int              `json:"version"`
	Entries      []map[string]any `json:"entries"`
	OriginalPath string           `json:"originalPath"`
}

func DefaultHome() (string, error) {
	if value := os.Getenv("CLAUDE_CONFIG_DIR"); value != "" {
		return expandHome(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude"), nil
}

func ResolveHome(home string) (string, error) {
	if home == "" {
		return DefaultHome()
	}
	return expandHome(home)
}

func ResolveSessionID(home, requested string) (string, error) {
	if requested != "" && requested != "current" {
		return requested, nil
	}
	for _, key := range []string{"CLAUDE_SESSION_ID", "CLAUDE_CODE_SESSION_ID"} {
		if value := os.Getenv(key); value != "" {
			return value, nil
		}
	}
	wd, _ := os.Getwd()
	if wd != "" {
		if id, err := latestSessionForCWD(home, wd); err == nil && id != "" {
			return id, nil
		}
	}
	id, err := latestSession(home)
	if err == nil && id != "" {
		return id, nil
	}
	return "", errors.New("no Claude session candidates found")
}

func ExportSession(opts ExportOptions) (*ExportData, error) {
	home, err := ResolveHome(opts.Home)
	if err != nil {
		return nil, err
	}
	sessionID, err := ResolveSessionID(home, opts.Session)
	if err != nil {
		return nil, err
	}
	sessionPath, err := findSessionPath(home, sessionID)
	if err != nil {
		return nil, err
	}
	sessionBytes, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, err
	}
	indexEntry, _ := findIndexEntry(home, sessionID)
	summary := SummarizeSession(sessionBytes)
	if summary.ID == "" {
		summary.ID = sessionID
	}
	if summary.CWD == "" {
		summary.CWD = stringValue(indexEntry["projectPath"])
	}
	title := stringValue(indexEntry["firstPrompt"])
	if title == "" {
		title = summary.FirstUserText
	}
	if title == "" {
		title = sessionID
	}
	rel, err := filepath.Rel(home, sessionPath)
	if err != nil {
		return nil, err
	}
	transcript := neutral.FromClaudeSession(sessionID, title, summary.CWD, sessionBytes)
	return &ExportData{
		SessionID:                 sessionID,
		Title:                     title,
		SourceHome:                home,
		SourceCWD:                 summary.CWD,
		SourceSessionRelativePath: filepath.ToSlash(rel),
		SessionBytes:              sessionBytes,
		IndexEntry:                cloneMap(indexEntry),
		Neutral:                   transcript,
		Summary:                   summary,
	}, nil
}

func RestoreSession(input RestoreInput, opts RestoreOptions) (*RestoreResult, error) {
	home, err := ResolveHome(opts.Home)
	if err != nil {
		return nil, err
	}
	targetCWD := opts.TargetCWD
	if targetCWD == "" {
		targetCWD, err = os.Getwd()
		if err != nil {
			return nil, err
		}
	}
	targetCWD, err = filepath.Abs(targetCWD)
	if err != nil {
		return nil, err
	}
	targetID := uuid.NewString()
	targetSessionPath := sessionPathForCWD(home, targetCWD, targetID)
	sidecarDir := filepath.Join(home, "agent-capsule-sources", targetID)
	result := &RestoreResult{
		SourceAgent:       input.SourceAgent,
		SourceSessionID:   input.SessionID,
		Status:            "planned",
		SessionID:         targetID,
		TargetHome:        home,
		TargetCWD:         targetCWD,
		TargetSessionPath: targetSessionPath,
		DryRun:            !opts.Execute,
		FallbackCommand:   fmt.Sprintf("cd %q && claude --session-id %s", targetCWD, targetID),
	}
	result.Writes = []string{targetSessionPath, filepath.Join(filepath.Dir(targetSessionPath), IndexFile)}
	if len(input.RawSidecar) > 0 || len(input.ManifestSidecar) > 0 || len(input.Neutral.Entries) > 0 {
		result.SidecarDir = sidecarDir
		result.Writes = append(result.Writes, filepath.Join(sidecarDir, "source.jsonl"), filepath.Join(sidecarDir, "neutral.json"))
		if len(input.ManifestSidecar) > 0 {
			result.Writes = append(result.Writes, filepath.Join(sidecarDir, "manifest.json"))
		}
	}
	if !opts.Execute {
		return result, nil
	}
	backupDir, err := backupState(home, targetID, targetSessionPath)
	if err != nil {
		return nil, err
	}
	result.BackupDir = backupDir
	sessionBytes := input.SessionBytes
	if strings.EqualFold(input.SourceAgent, "claude") && len(sessionBytes) > 0 {
		sessionBytes, err = rewriteSession(sessionBytes, targetID, targetCWD)
	} else {
		sessionBytes, err = synthesizeSession(input.Neutral, targetID, targetCWD)
	}
	if err != nil {
		return nil, err
	}
	if err := writeFileExclusive(targetSessionPath, sessionBytes); err != nil {
		result.Status = "needs_cli_fallback"
		result.Error = err.Error()
		return result, nil
	}
	if err := upsertProjectIndex(home, targetCWD, targetID, targetSessionPath, input.Title, sessionBytes); err != nil {
		result.Status = "needs_cli_fallback"
		result.Error = err.Error()
		return result, nil
	}
	if result.SidecarDir != "" {
		if err := writeSidecar(sidecarDir, input); err != nil {
			return nil, err
		}
	}
	result.Status = "ok"
	return result, nil
}

func VerifySession(home, sessionID, expectedCWD string) (*VerifyResult, error) {
	home, err := ResolveHome(home)
	if err != nil {
		return nil, err
	}
	sessionPath, err := findSessionPath(home, sessionID)
	sessionExists := err == nil && exists(sessionPath)
	indexEntry, _ := findIndexEntry(home, sessionID)
	cwds := []string{}
	if sessionExists {
		content, err := os.ReadFile(sessionPath)
		if err != nil {
			return nil, err
		}
		cwds = collectCWDs(content)
	}
	result := &VerifyResult{
		Status:            "ok",
		SessionID:         sessionID,
		TargetHome:        home,
		ExpectedCWD:       expectedCWD,
		SessionFileExists: sessionExists,
		IndexEntryExists:  len(indexEntry) > 0,
		SessionCWDs:       cwds,
	}
	if !result.SessionFileExists {
		result.Failures = append(result.Failures, "session file missing")
	}
	if !result.IndexEntryExists {
		result.Failures = append(result.Failures, "session index entry missing")
	}
	if expectedCWD != "" {
		expectedAbs, err := filepath.Abs(expectedCWD)
		if err == nil {
			expectedCWD = expectedAbs
		}
		if len(cwds) == 0 || !containsString(cwds, expectedCWD) {
			result.Failures = append(result.Failures, "session cwd mismatch")
		}
	}
	if len(result.Failures) > 0 {
		result.Status = "failed"
	}
	return result, nil
}

func SummarizeSession(content []byte) SessionSummary {
	var summary SessionSummary
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) != nil {
			continue
		}
		if id := stringValue(item["sessionId"]); id != "" {
			summary.ID = id
		}
		if cwd := stringValue(item["cwd"]); cwd != "" {
			summary.CWD = cwd
		}
		if ts := stringValue(item["timestamp"]); ts != "" {
			if summary.CreatedAt == "" {
				summary.CreatedAt = ts
			}
			summary.ModifiedAt = ts
		}
		if branch := stringValue(item["gitBranch"]); branch != "" {
			summary.GitBranch = branch
		}
		if boolValue(item["isMeta"]) {
			continue
		}
		message, _ := item["message"].(map[string]any)
		role := stringValue(message["role"])
		text := extractMessageText(message["content"])
		if text == "" {
			continue
		}
		if role == "user" && summary.FirstUserText == "" {
			summary.FirstUserText = text
		}
		if role == "assistant" {
			summary.LastAgentText = text
		}
	}
	return summary
}

func rewriteSession(content []byte, targetID, targetCWD string) ([]byte, error) {
	var items []map[string]any
	uuidMap := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) != nil {
			continue
		}
		if old := stringValue(item["uuid"]); old != "" {
			uuidMap[old] = uuid.NewString()
		}
		items = append(items, item)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	branch := currentGitBranch(targetCWD)
	var lines []string
	for _, item := range items {
		item["sessionId"] = targetID
		if _, ok := item["cwd"]; ok {
			item["cwd"] = targetCWD
		}
		if branch != "" {
			if _, ok := item["gitBranch"]; ok {
				item["gitBranch"] = branch
			}
		}
		if old := stringValue(item["uuid"]); old != "" {
			item["uuid"] = uuidMap[old]
		}
		if old := stringValue(item["parentUuid"]); old != "" {
			item["parentUuid"] = uuidMap[old]
		}
		if old := stringValue(item["leafUuid"]); old != "" {
			item["leafUuid"] = uuidMap[old]
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		lines = append(lines, string(encoded))
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func synthesizeSession(transcript neutral.Transcript, targetID, targetCWD string) ([]byte, error) {
	now := time.Now().UTC()
	branch := currentGitBranch(targetCWD)
	var lines []string
	var parent string
	add := func(item map[string]any) error {
		encoded, err := json.Marshal(item)
		if err != nil {
			return err
		}
		lines = append(lines, string(encoded))
		return nil
	}
	for _, operation := range []string{"enqueue", "dequeue"} {
		if err := add(map[string]any{
			"type":      "queue-operation",
			"operation": operation,
			"timestamp": now.Format(time.RFC3339Nano),
			"sessionId": targetID,
		}); err != nil {
			return nil, err
		}
	}
	entries := append([]neutral.Entry{}, transcript.Entries...)
	if len(entries) == 0 {
		entries = append(entries, neutral.Entry{Kind: "message", Role: "user", Text: neutral.HandoffText(transcript, "claude")})
	}
	for i, entry := range entries {
		id := uuid.NewString()
		item := map[string]any{
			"parentUuid":  parent,
			"isSidechain": false,
			"uuid":        id,
			"timestamp":   now.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano),
			"userType":    "external",
			"entrypoint":  "agent-capsule",
			"cwd":         targetCWD,
			"sessionId":   targetID,
			"version":     "agent-capsule",
			"gitBranch":   branch,
		}
		switch entry.Kind {
		case "message":
			role := entry.Role
			if role != "assistant" {
				role = "user"
			}
			item["type"] = role
			if role == "assistant" {
				item["message"] = map[string]any{
					"role":    "assistant",
					"model":   "<synthetic>",
					"content": []map[string]string{{"type": "text", "text": entry.Text}},
					"usage":   map[string]any{},
				}
			} else {
				item["message"] = map[string]any{"role": "user", "content": entry.Text}
			}
		case "tool":
			item["type"] = "assistant"
			item["message"] = map[string]any{
				"role":    "assistant",
				"model":   "<synthetic>",
				"content": []map[string]string{{"type": "text", "text": toolEvidenceText(entry)}},
				"usage":   map[string]any{},
			}
		default:
			continue
		}
		if err := add(item); err != nil {
			return nil, err
		}
		parent = id
	}
	if err := add(map[string]any{
		"type":       "last-prompt",
		"lastPrompt": neutral.HandoffText(transcript, "claude"),
		"leafUuid":   parent,
		"sessionId":  targetID,
	}); err != nil {
		return nil, err
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func upsertProjectIndex(home, targetCWD, sessionID, sessionPath, title string, sessionBytes []byte) error {
	indexPath := filepath.Join(projectDir(home, targetCWD), IndexFile)
	index, err := readProjectIndex(indexPath)
	if err != nil {
		return err
	}
	if index.Version == 0 {
		index.Version = 1
	}
	index.OriginalPath = targetCWD
	summary := SummarizeSession(sessionBytes)
	firstPrompt := title
	if firstPrompt == "" {
		firstPrompt = summary.FirstUserText
	}
	if firstPrompt == "" {
		firstPrompt = sessionID
	}
	info, _ := os.Stat(sessionPath)
	fileMtime := time.Now().UnixMilli()
	if info != nil {
		fileMtime = info.ModTime().UnixMilli()
	}
	entry := map[string]any{
		"sessionId":    sessionID,
		"fullPath":     sessionPath,
		"fileMtime":    fileMtime,
		"firstPrompt":  clip(firstPrompt, 240),
		"messageCount": countMessages(sessionBytes),
		"created":      fallbackString(summary.CreatedAt, time.Now().UTC().Format(time.RFC3339Nano)),
		"modified":     fallbackString(summary.ModifiedAt, time.Now().UTC().Format(time.RFC3339Nano)),
		"gitBranch":    currentGitBranch(targetCWD),
		"projectPath":  targetCWD,
		"isSidechain":  false,
	}
	replaced := false
	for i := range index.Entries {
		if stringValue(index.Entries[i]["sessionId"]) == sessionID {
			index.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		index.Entries = append([]map[string]any{entry}, index.Entries...)
	}
	payload, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(indexPath, append(payload, '\n'), 0o644)
}

func readProjectIndex(path string) (projectIndex, error) {
	var index projectIndex
	content, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return projectIndex{Version: 1}, nil
	}
	if err != nil {
		return index, err
	}
	if err := json.Unmarshal(content, &index); err != nil {
		return index, err
	}
	return index, nil
}

func findIndexEntry(home, sessionID string) (map[string]any, error) {
	projects := filepath.Join(home, ProjectsDir)
	var found map[string]any
	err := filepath.WalkDir(projects, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() != IndexFile || found != nil {
			return nil
		}
		index, err := readProjectIndex(path)
		if err != nil {
			return err
		}
		for _, entry := range index.Entries {
			if stringValue(entry["sessionId"]) == sessionID {
				found = cloneMap(entry)
				return nil
			}
		}
		return nil
	})
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	if found == nil {
		return map[string]any{}, nil
	}
	return found, nil
}

func findSessionPath(home, sessionID string) (string, error) {
	var matches []string
	base := filepath.Join(home, ProjectsDir)
	if !exists(base) {
		return "", fmt.Errorf("Claude projects directory not found: %s", base)
	}
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Name() == sessionID+".jsonl" {
			matches = append(matches, path)
		}
		return nil
	})
	sort.Strings(matches)
	if len(matches) == 0 {
		return "", fmt.Errorf("Claude session file not found for session %s", sessionID)
	}
	return matches[0], nil
}

func latestSessionForCWD(home, cwd string) (string, error) {
	indexPath := filepath.Join(projectDir(home, cwd), IndexFile)
	index, err := readProjectIndex(indexPath)
	if err == nil && len(index.Entries) > 0 {
		sort.SliceStable(index.Entries, func(i, j int) bool {
			return numberValue(index.Entries[i]["fileMtime"]) > numberValue(index.Entries[j]["fileMtime"])
		})
		if id := stringValue(index.Entries[0]["sessionId"]); id != "" {
			return id, nil
		}
	}
	return latestSessionFile(filepath.Dir(indexPath))
}

func latestSession(home string) (string, error) {
	var candidates []fileCandidate
	base := filepath.Join(home, ProjectsDir)
	if !exists(base) {
		return "", os.ErrNotExist
	}
	_ = filepath.WalkDir(base, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		candidates = append(candidates, fileCandidate{ID: strings.TrimSuffix(d.Name(), ".jsonl"), ModTime: info.ModTime()})
		return nil
	})
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ModTime.After(candidates[j].ModTime)
	})
	if len(candidates) == 0 {
		return "", os.ErrNotExist
	}
	return candidates[0].ID, nil
}

type fileCandidate struct {
	ID      string
	ModTime time.Time
}

func latestSessionFile(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var candidates []fileCandidate
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		candidates = append(candidates, fileCandidate{ID: strings.TrimSuffix(entry.Name(), ".jsonl"), ModTime: info.ModTime()})
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].ModTime.After(candidates[j].ModTime)
	})
	if len(candidates) == 0 {
		return "", os.ErrNotExist
	}
	return candidates[0].ID, nil
}

func sessionPathForCWD(home, cwd, sessionID string) string {
	return filepath.Join(projectDir(home, cwd), sessionID+".jsonl")
}

func projectDir(home, cwd string) string {
	return filepath.Join(home, ProjectsDir, ProjectDirName(cwd))
}

func ProjectDirName(cwd string) string {
	abs, err := filepath.Abs(cwd)
	if err == nil {
		cwd = abs
	}
	return strings.ReplaceAll(cwd, string(filepath.Separator), "-")
}

func collectCWDs(content []byte) []string {
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal([]byte(scanner.Text()), &item) != nil {
			continue
		}
		if cwd := stringValue(item["cwd"]); cwd != "" {
			seen[cwd] = true
		}
	}
	var values []string
	for value := range seen {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

func countMessages(content []byte) int {
	count := 0
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal([]byte(scanner.Text()), &item) == nil {
			if typ := stringValue(item["type"]); typ == "user" || typ == "assistant" {
				count++
			}
		}
	}
	return count
}

func writeSidecar(dir string, input RestoreInput) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if len(input.RawSidecar) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "source.jsonl"), input.RawSidecar, 0o644); err != nil {
			return err
		}
	}
	if len(input.ManifestSidecar) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), input.ManifestSidecar, 0o644); err != nil {
			return err
		}
	}
	if len(input.Neutral.Entries) > 0 {
		payload, err := json.MarshalIndent(input.Neutral, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "neutral.json"), append(payload, '\n'), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func backupState(home, sessionID, targetSessionPath string) (string, error) {
	dir := filepath.Join(home, "backups_state", "agent-capsule", time.Now().UTC().Format("20060102T150405Z")+"-"+sessionID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	indexPath := filepath.Join(filepath.Dir(targetSessionPath), IndexFile)
	if exists(indexPath) {
		if err := copyFile(indexPath, filepath.Join(dir, IndexFile)); err != nil {
			return "", err
		}
	}
	if exists(targetSessionPath) {
		if err := copyFile(targetSessionPath, filepath.Join(dir, "existing-session.jsonl")); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func toolEvidenceText(entry neutral.Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Tool evidence: %s", entry.Tool)
	if entry.Status != "" {
		fmt.Fprintf(&b, " (%s)", entry.Status)
	}
	if entry.Input != "" {
		fmt.Fprintf(&b, "\ninput: %s", entry.Input)
	}
	if entry.Output != "" {
		fmt.Fprintf(&b, "\noutput: %s", entry.Output)
	}
	return b.String()
}

func extractMessageText(content any) string {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value)
	case []any:
		var parts []string
		for _, item := range value {
			m, _ := item.(map[string]any)
			if text := stringValue(m["text"]); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n"))
	default:
		return ""
	}
}

func currentGitBranch(cwd string) string {
	head, err := os.ReadFile(filepath.Join(cwd, ".git", "HEAD"))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(head))
	if strings.HasPrefix(text, "ref: refs/heads/") {
		return strings.TrimPrefix(text, "ref: refs/heads/")
	}
	return ""
}

func expandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return filepath.Abs(path)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func writeFileExclusive(path string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.Write(content)
	return err
}

func copyFile(source, dest string) error {
	content, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	return os.WriteFile(dest, content, 0o644)
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func clip(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max] + "..."
}

func fallbackString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	default:
		return ""
	}
}

func numberValue(value any) int64 {
	switch v := value.(type) {
	case int64:
		return v
	case int:
		return int64(v)
	case float64:
		return int64(v)
	default:
		return 0
	}
}

func boolValue(value any) bool {
	v, _ := value.(bool)
	return v
}
