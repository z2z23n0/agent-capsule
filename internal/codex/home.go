package codex

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const (
	SessionIndexFile = "session_index.jsonl"
	StateSQLiteFile  = "state_5.sqlite"
)

type ExportData struct {
	ThreadID                  string
	Title                     string
	SourceHome                string
	SourceCWD                 string
	SourceSessionRelativePath string
	SessionBytes              []byte
	IndexEntry                map[string]any
	ThreadRow                 map[string]any
	Summary                   SessionSummary
}

type SessionSummary struct {
	ID             string
	CWD            string
	Timestamp      string
	LastTimestamp  string
	CLIVersion     string
	Source         string
	ThreadSource   string
	ModelProvider  string
	FirstUserText  string
	LastAgentText  string
	VisibleExcerpt []string
}

type RestoreInput struct {
	ThreadID                  string
	Title                     string
	SourceSessionRelativePath string
	SessionBytes              []byte
	IndexEntry                map[string]any
	ThreadRow                 map[string]any
}

type RestoreOptions struct {
	Home      string
	TargetCWD string
	Execute   bool
}

type RestoreResult struct {
	SourceThreadID    string   `json:"source_thread_id"`
	Status            string   `json:"status"`
	ThreadID          string   `json:"thread_id"`
	TargetHome        string   `json:"target_home"`
	TargetCWD         string   `json:"target_cwd"`
	TargetSessionPath string   `json:"target_session_path"`
	BackupDir         string   `json:"backup_dir,omitempty"`
	Writes            []string `json:"writes"`
	DryRun            bool     `json:"dry_run"`
}

type VerifyResult struct {
	Status            string   `json:"status"`
	ThreadID          string   `json:"thread_id"`
	TargetHome        string   `json:"target_home"`
	ExpectedCWD       string   `json:"expected_cwd,omitempty"`
	SessionFileExists bool     `json:"session_file_exists"`
	IndexEntryExists  bool     `json:"index_entry_exists"`
	SQLiteRowExists   bool     `json:"sqlite_row_exists"`
	SessionCWDs       []string `json:"session_cwds"`
	SQLiteCWD         string   `json:"sqlite_cwd,omitempty"`
	Failures          []string `json:"failures,omitempty"`
}

func DefaultHome() (string, error) {
	if value := os.Getenv("CODEX_HOME"); value != "" {
		return expandHome(value)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex"), nil
}

func ResolveHome(home string) (string, error) {
	if home == "" {
		return DefaultHome()
	}
	return expandHome(home)
}

func ResolveThreadID(home, requested string) (string, error) {
	if requested != "" && requested != "current" {
		return requested, nil
	}
	for _, key := range []string{"CODEX_THREAD_ID", "CODEX_SESSION_ID"} {
		if value := os.Getenv(key); value != "" {
			return value, nil
		}
	}
	wd, _ := os.Getwd()
	if wd != "" {
		if id, err := latestSQLiteThreadForCWD(home, wd); err == nil && id != "" {
			return id, nil
		}
	}
	if id, err := latestSQLiteThread(home); err == nil && id != "" {
		return id, nil
	}
	index, err := readSessionIndex(home)
	if err != nil {
		return "", err
	}
	if len(index) == 0 {
		return "", errors.New("no thread candidates found in Codex home")
	}
	sort.SliceStable(index, func(i, j int) bool {
		return stringValue(index[i]["updated_at"]) > stringValue(index[j]["updated_at"])
	})
	return stringValue(index[0]["id"]), nil
}

func ExportThread(home, threadID string) (*ExportData, error) {
	home, err := ResolveHome(home)
	if err != nil {
		return nil, err
	}
	indexEntry, err := findIndexEntry(home, threadID)
	if err != nil {
		return nil, err
	}
	threadRow, err := readSQLiteThreadRow(home, threadID)
	if err != nil {
		return nil, err
	}
	sessionPath, err := findSessionPath(home, threadID, threadRow)
	if err != nil {
		return nil, err
	}
	sessionBytes, err := os.ReadFile(sessionPath)
	if err != nil {
		return nil, err
	}
	summary := SummarizeSession(sessionBytes)
	rel, err := filepath.Rel(home, sessionPath)
	if err != nil {
		return nil, err
	}
	title := stringValue(indexEntry["thread_name"])
	if title == "" {
		title = stringValue(threadRow["title"])
	}
	if title == "" {
		title = threadID
	}
	sourceCWD := stringValue(threadRow["cwd"])
	if sourceCWD == "" {
		sourceCWD = summary.CWD
	}
	return &ExportData{
		ThreadID:                  threadID,
		Title:                     title,
		SourceHome:                home,
		SourceCWD:                 sourceCWD,
		SourceSessionRelativePath: filepath.ToSlash(rel),
		SessionBytes:              sessionBytes,
		IndexEntry:                cloneMap(indexEntry),
		ThreadRow:                 cloneMap(threadRow),
		Summary:                   summary,
	}, nil
}

func RestoreThread(input RestoreInput, opts RestoreOptions) (*RestoreResult, error) {
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
	targetThreadID, targetSessionPath, err := newImportTarget(home)
	if err != nil {
		return nil, err
	}
	result := &RestoreResult{
		SourceThreadID:    input.ThreadID,
		Status:            "planned",
		ThreadID:          targetThreadID,
		TargetHome:        home,
		TargetCWD:         targetCWD,
		TargetSessionPath: targetSessionPath,
		DryRun:            !opts.Execute,
	}
	result.Writes = plannedWrites(home, targetSessionPath)
	if !opts.Execute {
		return result, nil
	}
	backupDir, err := backupState(home, targetThreadID, targetSessionPath)
	if err != nil {
		return nil, err
	}
	result.BackupDir = backupDir
	if err := os.MkdirAll(filepath.Dir(targetSessionPath), 0o755); err != nil {
		return nil, err
	}
	rewritten, err := RewriteSessionForImport(input.SessionBytes, targetCWD, targetThreadID)
	if err != nil {
		return nil, err
	}
	if err := writeFileExclusive(targetSessionPath, rewritten); err != nil {
		return nil, err
	}
	targetInput := input
	targetInput.ThreadID = targetThreadID
	if err := upsertSessionIndex(home, targetThreadID, input.Title, input.IndexEntry); err != nil {
		return nil, err
	}
	if exists(filepath.Join(home, StateSQLiteFile)) {
		if err := upsertSQLiteThread(home, targetInput, targetSessionPath, targetCWD); err != nil {
			return nil, err
		}
	}
	result.Status = "ok"
	return result, nil
}

func VerifyThread(home, threadID, expectedCWD string) (*VerifyResult, error) {
	home, err := ResolveHome(home)
	if err != nil {
		return nil, err
	}
	row, _ := readSQLiteThreadRow(home, threadID)
	sessionPath, err := findSessionPath(home, threadID, row)
	sessionExists := err == nil && exists(sessionPath)
	indexEntry, _ := findIndexEntry(home, threadID)
	cwds := []string{}
	if sessionExists {
		content, err := os.ReadFile(sessionPath)
		if err != nil {
			return nil, err
		}
		cwds = collectSessionCWDs(content)
	}
	result := &VerifyResult{
		Status:            "ok",
		ThreadID:          threadID,
		TargetHome:        home,
		ExpectedCWD:       expectedCWD,
		SessionFileExists: sessionExists,
		IndexEntryExists:  len(indexEntry) > 0,
		SQLiteRowExists:   len(row) > 0,
		SessionCWDs:       cwds,
		SQLiteCWD:         stringValue(row["cwd"]),
	}
	if !result.SessionFileExists {
		result.Failures = append(result.Failures, "session file missing")
	}
	if !result.IndexEntryExists {
		result.Failures = append(result.Failures, "session index entry missing")
	}
	if exists(filepath.Join(home, StateSQLiteFile)) && !result.SQLiteRowExists {
		result.Failures = append(result.Failures, "sqlite thread row missing")
	}
	if expectedCWD != "" {
		expectedAbs, err := filepath.Abs(expectedCWD)
		if err == nil {
			expectedCWD = expectedAbs
		}
		if len(cwds) != 1 || cwds[0] != expectedCWD {
			result.Failures = append(result.Failures, "session cwd mismatch")
		}
		if result.SQLiteRowExists && result.SQLiteCWD != expectedCWD {
			result.Failures = append(result.Failures, "sqlite cwd mismatch")
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
		if ts := stringValue(item["timestamp"]); ts != "" {
			summary.LastTimestamp = ts
		}
		payload, _ := item["payload"].(map[string]any)
		switch stringValue(item["type"]) {
		case "session_meta":
			summary.ID = stringValue(payload["id"])
			summary.CWD = stringValue(payload["cwd"])
			summary.Timestamp = stringValue(payload["timestamp"])
			summary.CLIVersion = stringValue(payload["cli_version"])
			summary.Source = stringValue(payload["source"])
			summary.ThreadSource = stringValue(payload["thread_source"])
			summary.ModelProvider = stringValue(payload["model_provider"])
		case "turn_context":
			if summary.CWD == "" {
				summary.CWD = stringValue(payload["cwd"])
			}
		case "response_item":
			if stringValue(payload["type"]) == "message" {
				role := stringValue(payload["role"])
				text := extractMessageText(payload["content"])
				if text != "" {
					if role == "user" && summary.FirstUserText == "" {
						summary.FirstUserText = text
					}
					if role == "assistant" {
						summary.LastAgentText = text
					}
					if len(summary.VisibleExcerpt) < 8 {
						summary.VisibleExcerpt = append(summary.VisibleExcerpt, role+": "+clip(text, 600))
					}
				}
			}
		case "event_msg":
			if stringValue(payload["type"]) == "agent_message" {
				text := stringValue(payload["message"])
				if text != "" {
					summary.LastAgentText = text
					if len(summary.VisibleExcerpt) < 8 {
						summary.VisibleExcerpt = append(summary.VisibleExcerpt, "assistant: "+clip(text, 600))
					}
				}
			}
		}
	}
	return summary
}

func RewriteSessionForImport(content []byte, targetCWD, targetThreadID string) ([]byte, error) {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		var item map[string]any
		if json.Unmarshal([]byte(line), &item) != nil {
			lines = append(lines, line)
			continue
		}
		typ := stringValue(item["type"])
		if typ == "session_meta" || typ == "turn_context" {
			payload, _ := item["payload"].(map[string]any)
			if payload != nil {
				payload["cwd"] = targetCWD
				if typ == "session_meta" && targetThreadID != "" {
					payload["id"] = targetThreadID
				}
			}
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		lines = append(lines, string(encoded))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func RewriteSessionCWD(content []byte, targetCWD string) ([]byte, error) {
	return RewriteSessionForImport(content, targetCWD, "")
}

func collectSessionCWDs(content []byte) []string {
	seen := map[string]bool{}
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var item map[string]any
		if json.Unmarshal([]byte(scanner.Text()), &item) != nil {
			continue
		}
		typ := stringValue(item["type"])
		if typ != "session_meta" && typ != "turn_context" {
			continue
		}
		payload, _ := item["payload"].(map[string]any)
		cwd := stringValue(payload["cwd"])
		if cwd != "" {
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

func readSessionIndex(home string) ([]map[string]any, error) {
	path := filepath.Join(home, SessionIndexFile)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var entries []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		entries = append(entries, entry)
	}
	return entries, scanner.Err()
}

func findIndexEntry(home, threadID string) (map[string]any, error) {
	entries, err := readSessionIndex(home)
	if err != nil {
		return nil, err
	}
	for _, entry := range entries {
		if stringValue(entry["id"]) == threadID {
			return entry, nil
		}
	}
	return map[string]any{}, nil
}

func upsertSessionIndex(home, threadID, title string, source map[string]any) error {
	entries, err := readSessionIndex(home)
	if err != nil {
		return err
	}
	entry := cloneMap(source)
	if entry == nil {
		entry = map[string]any{}
	}
	entry["id"] = threadID
	if stringValue(entry["thread_name"]) == "" {
		if title == "" {
			title = threadID
		}
		entry["thread_name"] = title
	}
	entry["updated_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	replaced := false
	for i := range entries {
		if stringValue(entries[i]["id"]) == threadID {
			entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		entries = append(entries, entry)
	}
	path := filepath.Join(home, SessionIndexFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.Create(path)
	if err != nil {
		return err
	}
	defer file.Close()
	enc := json.NewEncoder(file)
	enc.SetEscapeHTML(false)
	for _, item := range entries {
		if err := enc.Encode(item); err != nil {
			return err
		}
	}
	return nil
}

func findSessionPath(home, threadID string, row map[string]any) (string, error) {
	if rollout := stringValue(row["rollout_path"]); rollout != "" && exists(rollout) {
		return rollout, nil
	}
	var matches []string
	for _, root := range []string{"sessions", "archived_sessions"} {
		base := filepath.Join(home, root)
		if !exists(base) {
			continue
		}
		_ = filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			name := d.Name()
			if strings.Contains(name, threadID) && strings.HasSuffix(name, ".jsonl") {
				matches = append(matches, path)
			}
			return nil
		})
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		return "", fmt.Errorf("session file not found for thread %s", threadID)
	}
	return matches[0], nil
}

func latestSQLiteThreadForCWD(home, cwd string) (string, error) {
	return latestSQLiteThreadWhere(home, "where cwd = ?", cwd)
}

func latestSQLiteThread(home string) (string, error) {
	return latestSQLiteThreadWhere(home, "", "")
}

func latestSQLiteThreadWhere(home, where, arg string) (string, error) {
	dbPath := filepath.Join(home, StateSQLiteFile)
	if !exists(dbPath) {
		return "", os.ErrNotExist
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	orderBy := "id desc"
	names, err := sqliteColumnNames(db)
	if err == nil {
		if names["updated_at_ms"] {
			orderBy = "updated_at_ms desc"
		} else if names["updated_at"] {
			orderBy = "updated_at desc"
		}
	}
	query := "select id from threads " + where + " order by " + orderBy + " limit 1"
	var row *sql.Row
	if where == "" {
		row = db.QueryRow(query)
	} else {
		row = db.QueryRow(query, arg)
	}
	var id string
	if err := row.Scan(&id); err != nil {
		return "", err
	}
	return id, nil
}

func readSQLiteThreadRow(home, threadID string) (map[string]any, error) {
	dbPath := filepath.Join(home, StateSQLiteFile)
	if !exists(dbPath) {
		return map[string]any{}, nil
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("select * from threads where id = ?", threadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return map[string]any{}, nil
	}
	columns, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	values := make([]any, len(columns))
	ptrs := make([]any, len(columns))
	for i := range values {
		ptrs[i] = &values[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		return nil, err
	}
	result := map[string]any{}
	for i, column := range columns {
		switch value := values[i].(type) {
		case []byte:
			result[column] = string(value)
		default:
			result[column] = value
		}
	}
	return result, rows.Err()
}

type sqliteColumn struct {
	Name       string
	NotNull    bool
	PrimaryKey bool
	Default    any
}

func upsertSQLiteThread(home string, input RestoreInput, targetSessionPath, targetCWD string) error {
	dbPath := filepath.Join(home, StateSQLiteFile)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	columns, err := sqliteThreadColumns(db)
	if err != nil {
		return err
	}
	now := time.Now()
	row := deriveSQLiteRow(input, targetSessionPath, targetCWD, now)
	var names []string
	for _, column := range columns {
		if _, ok := row[column.Name]; ok {
			names = append(names, column.Name)
			continue
		}
		if column.NotNull && !column.PrimaryKey && column.Default == nil {
			if value, ok := knownSQLiteDefault(column.Name); ok {
				row[column.Name] = value
				names = append(names, column.Name)
			}
		}
	}
	if len(names) == 0 {
		return errors.New("target sqlite threads schema has no compatible columns")
	}
	placeholders := make([]string, len(names))
	update := make([]string, 0, len(names)-1)
	values := make([]any, len(names))
	for i, name := range names {
		placeholders[i] = "?"
		values[i] = row[name]
		if name != "id" {
			update = append(update, fmt.Sprintf("%s=excluded.%s", name, name))
		}
	}
	query := fmt.Sprintf(
		"insert into threads (%s) values (%s) on conflict(id) do update set %s",
		strings.Join(names, ","),
		strings.Join(placeholders, ","),
		strings.Join(update, ","),
	)
	_, err = db.Exec(query, values...)
	return err
}

func sqliteThreadColumns(db *sql.DB) ([]sqliteColumn, error) {
	rows, err := db.Query("pragma table_info(threads)")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []sqliteColumn
	for rows.Next() {
		var cid int
		var name, typ string
		var notNull int
		var defaultValue any
		var pk int
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			return nil, err
		}
		columns = append(columns, sqliteColumn{
			Name:       name,
			NotNull:    notNull == 1,
			PrimaryKey: pk == 1,
			Default:    defaultValue,
		})
	}
	return columns, rows.Err()
}

func sqliteColumnNames(db *sql.DB) (map[string]bool, error) {
	columns, err := sqliteThreadColumns(db)
	if err != nil {
		return nil, err
	}
	names := make(map[string]bool, len(columns))
	for _, column := range columns {
		names[column.Name] = true
	}
	return names, nil
}

func deriveSQLiteRow(input RestoreInput, targetSessionPath, targetCWD string, now time.Time) map[string]any {
	row := cloneMap(input.ThreadRow)
	if row == nil {
		row = map[string]any{}
	}
	nowUnix := now.Unix()
	nowMS := now.UnixMilli()
	row["id"] = input.ThreadID
	row["rollout_path"] = targetSessionPath
	row["cwd"] = targetCWD
	row["updated_at"] = nowUnix
	row["updated_at_ms"] = nowMS
	if _, ok := row["created_at"]; !ok || row["created_at"] == nil {
		row["created_at"] = nowUnix
	}
	if _, ok := row["created_at_ms"]; !ok || row["created_at_ms"] == nil {
		row["created_at_ms"] = toInt64(row["created_at"], nowUnix) * 1000
	}
	if stringValue(row["title"]) == "" {
		row["title"] = input.Title
	}
	if stringValue(row["title"]) == "" {
		row["title"] = input.ThreadID
	}
	defaults := map[string]any{
		"source":             "imported",
		"model_provider":     "openai",
		"sandbox_policy":     "{}",
		"approval_mode":      "never",
		"tokens_used":        int64(0),
		"has_user_event":     int64(0),
		"archived":           int64(0),
		"cli_version":        "",
		"first_user_message": row["title"],
		"memory_mode":        "enabled",
		"preview":            row["title"],
	}
	for key, value := range defaults {
		if _, ok := row[key]; !ok || row[key] == nil {
			row[key] = value
		}
	}
	if toInt64(row["archived"], 0) == 0 {
		row["archived_at"] = nil
	}
	return row
}

func knownSQLiteDefault(name string) (any, bool) {
	defaults := map[string]any{
		"source":             "imported",
		"model_provider":     "openai",
		"cwd":                "",
		"title":              "",
		"sandbox_policy":     "{}",
		"approval_mode":      "never",
		"tokens_used":        int64(0),
		"has_user_event":     int64(0),
		"archived":           int64(0),
		"cli_version":        "",
		"first_user_message": "",
		"memory_mode":        "enabled",
		"preview":            "",
	}
	value, ok := defaults[name]
	return value, ok
}

func backupState(home, threadID, targetSessionPath string) (string, error) {
	dir := filepath.Join(home, "backups_state", "agent-capsule", time.Now().UTC().Format("20060102T150405Z")+"-"+threadID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for _, name := range []string{SessionIndexFile, StateSQLiteFile, StateSQLiteFile + "-wal", StateSQLiteFile + "-shm"} {
		source := filepath.Join(home, name)
		if exists(source) {
			if err := copyFile(source, filepath.Join(dir, filepath.Base(source))); err != nil {
				return "", err
			}
		}
	}
	if exists(targetSessionPath) {
		if err := copyFile(targetSessionPath, filepath.Join(dir, "existing-session.jsonl")); err != nil {
			return "", err
		}
	}
	return dir, nil
}

func newImportTarget(home string) (string, string, error) {
	for range 10 {
		id, err := newThreadID()
		if err != nil {
			return "", "", err
		}
		path := newSessionPath(home, id, time.Now().UTC())
		if exists(path) {
			continue
		}
		if entry, err := findIndexEntry(home, id); err != nil {
			return "", "", err
		} else if len(entry) > 0 {
			continue
		}
		if row, err := readSQLiteThreadRow(home, id); err != nil {
			return "", "", err
		} else if len(row) > 0 {
			continue
		}
		return id, path, nil
	}
	return "", "", errors.New("could not allocate a unique imported thread id")
}

func newThreadID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", err
	}
	return id.String(), nil
}

func newSessionPath(home, threadID string, now time.Time) string {
	return filepath.Join(
		home,
		"sessions",
		now.Format("2006"),
		now.Format("01"),
		now.Format("02"),
		fmt.Sprintf("rollout-%s-%s.jsonl", now.Format("2006-01-02T15-04-05"), threadID),
	)
}

func plannedWrites(home, targetSessionPath string) []string {
	writes := []string{targetSessionPath, filepath.Join(home, SessionIndexFile)}
	if exists(filepath.Join(home, StateSQLiteFile)) {
		writes = append(writes, filepath.Join(home, StateSQLiteFile))
	}
	return writes
}

func safeJoin(home, rel string) (string, error) {
	if rel == "" {
		return "", errors.New("missing source session relative path")
	}
	rel = filepath.FromSlash(rel)
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("unsafe absolute session path: %s", rel)
	}
	clean := filepath.Clean(rel)
	if clean == "." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || clean == ".." {
		return "", fmt.Errorf("unsafe session path: %s", rel)
	}
	return filepath.Join(home, clean), nil
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

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	case fmt.Stringer:
		return v.String()
	default:
		return ""
	}
}

func toInt64(value any, fallback int64) int64 {
	switch v := value.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return n
		}
	case string:
		var n int64
		if _, err := fmt.Sscan(v, &n); err == nil {
			return n
		}
	}
	return fallback
}

func extractMessageText(content any) string {
	items, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range items {
		m, _ := item.(map[string]any)
		for _, key := range []string{"text", "output_text"} {
			if text := stringValue(m[key]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func clip(text string, max int) string {
	text = strings.TrimSpace(text)
	if len(text) <= max {
		return text
	}
	return text[:max] + "..."
}
