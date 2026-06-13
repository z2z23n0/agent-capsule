package capsule

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/z2z23n0/agent-capsule/internal/claude"
	"github.com/z2z23n0/agent-capsule/internal/codex"
	"github.com/z2z23n0/agent-capsule/internal/neutral"
)

type ImportOptions struct {
	Target         string
	Home           string
	TargetCWD      string
	Execute        bool
	AllowModelCall bool
}

type HandoffOptions struct {
	From           string
	To             string
	SourceHome     string
	TargetHome     string
	SourceThread   string
	TargetCWD      string
	Execute        bool
	AllowModelCall bool
}

type HandoffResult struct {
	Status     string     `json:"status"`
	From       string     `json:"from"`
	To         string     `json:"to"`
	SourceID   string     `json:"source_id"`
	TargetID   string     `json:"target_id,omitempty"`
	TargetHome string     `json:"target_home,omitempty"`
	TargetCWD  string     `json:"target_cwd,omitempty"`
	Safety     SafetyScan `json:"safety"`
	Result     any        `json:"result,omitempty"`
	Warnings   []string   `json:"warnings,omitempty"`
	DryRun     bool       `json:"dry_run"`
}

func ImportAny(source string, opts ImportOptions) (any, error) {
	if isHTTPURL(source) {
		return ImportFromURL(source, opts)
	}
	return Import(source, opts)
}

func Import(path string, opts ImportOptions) (any, error) {
	loaded, err := load(path)
	if err != nil {
		return nil, err
	}
	return restoreLoaded(loaded, opts)
}

func ImportFromURL(rawURL string, opts ImportOptions) (any, error) {
	tempPath, err := downloadLinkedCapsule(rawURL)
	if err != nil {
		return nil, err
	}
	defer os.Remove(tempPath)
	return Import(tempPath, opts)
}

func Handoff(opts HandoffOptions) (*HandoffResult, error) {
	from := normalizeAgent(opts.From, "auto")
	if from == "auto" {
		from = detectSourceAgent(opts)
	}
	to := normalizeAgent(opts.To, "")
	if to == "" {
		return nil, errors.New("missing target agent")
	}
	result := &HandoffResult{
		Status: "planned",
		From:   from,
		To:     to,
		DryRun: !opts.Execute,
	}
	switch from {
	case AgentCodex:
		sourceHome, err := codex.ResolveHome(opts.SourceHome)
		if err != nil {
			return nil, err
		}
		threadID, err := codex.ResolveThreadID(sourceHome, opts.SourceThread)
		if err != nil {
			return nil, err
		}
		data, err := codex.ExportThreadWithOptions(sourceHome, threadID, codex.ExportThreadOptions{
			DropSelfExportTurn: shouldDropSelfExportTurn(opts.SourceThread, threadID),
		})
		if err != nil {
			return nil, err
		}
		transcript := neutral.FromCodexSession(data.ThreadID, data.Title, data.SourceCWD, data.SessionBytes)
		result.SourceID = data.ThreadID
		result.Safety = ScanSecrets(data.SessionBytes)
		if len(result.Safety.Findings) > 0 {
			result.Warnings = append(result.Warnings, "source session contains high-confidence secret scan findings; local handoff continued without creating a share artifact")
		}
		restored, err := restoreDirectToTarget(to, opts, directPayload{
			SourceAgent: AgentCodex,
			SourceID:    data.ThreadID,
			Title:       data.Title,
			Raw:         data.SessionBytes,
			Neutral:     transcript,
			CodexData:   data,
		})
		if err != nil {
			return nil, err
		}
		result.Result = restored
		result.Status, result.TargetID, result.TargetHome, result.TargetCWD = resultFields(restored)
		return result, nil
	case AgentClaude:
		data, err := claude.ExportSession(claude.ExportOptions{Home: opts.SourceHome, Session: opts.SourceThread})
		if err != nil {
			return nil, err
		}
		result.SourceID = data.SessionID
		result.Safety = ScanSecrets(data.SessionBytes)
		if len(result.Safety.Findings) > 0 {
			result.Warnings = append(result.Warnings, "source session contains high-confidence secret scan findings; local handoff continued without creating a share artifact")
		}
		restored, err := restoreDirectToTarget(to, opts, directPayload{
			SourceAgent: AgentClaude,
			SourceID:    data.SessionID,
			Title:       data.Title,
			Raw:         data.SessionBytes,
			Neutral:     data.Neutral,
		})
		if err != nil {
			return nil, err
		}
		result.Result = restored
		result.Status, result.TargetID, result.TargetHome, result.TargetCWD = resultFields(restored)
		return result, nil
	default:
		return nil, fmt.Errorf("unsupported source agent %q", from)
	}
}

type directPayload struct {
	SourceAgent string
	SourceID    string
	Title       string
	Raw         []byte
	Neutral     neutral.Transcript
	CodexData   *codex.ExportData
}

func restoreLoaded(loaded *loadedCapsule, opts ImportOptions) (any, error) {
	target := normalizeAgent(opts.Target, AgentCodex)
	switch target {
	case AgentCodex:
		return restoreLoadedToCodex(loaded, codex.RestoreOptions{Home: opts.Home, TargetCWD: opts.TargetCWD, Execute: opts.Execute})
	case AgentClaude:
		return restoreLoadedToClaude(loaded, claude.RestoreOptions{Home: opts.Home, TargetCWD: opts.TargetCWD, Execute: opts.Execute})
	default:
		return nil, fmt.Errorf("unsupported target %q", opts.Target)
	}
}

func restoreLoadedToCodex(loaded *loadedCapsule, opts codex.RestoreOptions) (*codex.RestoreResult, error) {
	source := normalizeAgent(loaded.Manifest.SourceAgent, AgentCodex)
	if source == AgentCodex {
		return restoreCodexNative(loaded, opts)
	}
	return restoreCodexFromNeutral(loaded.Neutral, loaded.ClaudeSession, loaded.RawPayloads["manifest.json"], opts)
}

func restoreLoadedToClaude(loaded *loadedCapsule, opts claude.RestoreOptions) (*claude.RestoreResult, error) {
	source := normalizeAgent(loaded.Manifest.SourceAgent, AgentCodex)
	input := claude.RestoreInput{
		SourceAgent:               source,
		SessionID:                 loaded.Manifest.ThreadID,
		Title:                     loaded.Manifest.ThreadTitle,
		SourceSessionRelativePath: loaded.Manifest.SourceSessionRelativePath,
		Neutral:                   loaded.Neutral,
		ManifestSidecar:           loaded.RawPayloads["manifest.json"],
	}
	if source == AgentClaude {
		input.SessionBytes = loaded.ClaudeSession
		input.RawSidecar = loaded.ClaudeSession
	} else {
		input.RawSidecar = loaded.Session
	}
	return claude.RestoreSession(input, opts)
}

func restoreDirectToTarget(target string, opts HandoffOptions, payload directPayload) (any, error) {
	switch target {
	case AgentCodex:
		if payload.SourceAgent == AgentCodex && payload.CodexData != nil {
			input := codex.RestoreInput{
				ThreadID:                  payload.CodexData.ThreadID,
				Title:                     payload.CodexData.Title,
				SourceSessionRelativePath: payload.CodexData.SourceSessionRelativePath,
				SessionBytes:              payload.CodexData.SessionBytes,
				IndexEntry:                payload.CodexData.IndexEntry,
				ThreadRow:                 payload.CodexData.ThreadRow,
			}
			return codex.RestoreThread(input, codex.RestoreOptions{Home: opts.TargetHome, TargetCWD: opts.TargetCWD, Execute: opts.Execute})
		}
		return restoreCodexFromNeutral(payload.Neutral, payload.Raw, nil, codex.RestoreOptions{Home: opts.TargetHome, TargetCWD: opts.TargetCWD, Execute: opts.Execute})
	case AgentClaude:
		input := claude.RestoreInput{
			SourceAgent: payload.SourceAgent,
			SessionID:   payload.SourceID,
			Title:       payload.Title,
			Neutral:     payload.Neutral,
			RawSidecar:  payload.Raw,
		}
		if payload.SourceAgent == AgentClaude {
			input.SessionBytes = payload.Raw
		}
		return claude.RestoreSession(input, claude.RestoreOptions{Home: opts.TargetHome, TargetCWD: opts.TargetCWD, Execute: opts.Execute})
	default:
		return nil, fmt.Errorf("unsupported target agent %q", target)
	}
}

func restoreCodexNative(loaded *loadedCapsule, opts codex.RestoreOptions) (*codex.RestoreResult, error) {
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

func restoreCodexFromNeutral(transcript neutral.Transcript, rawSource, manifestPayload []byte, opts codex.RestoreOptions) (*codex.RestoreResult, error) {
	sessionBytes, err := codexSessionFromNeutral(transcript)
	if err != nil {
		return nil, err
	}
	input := codex.RestoreInput{
		ThreadID:     fallbackString(transcript.SourceID, "imported-cross-agent"),
		Title:        fallbackString(transcript.Title, "Cross-agent handoff"),
		SessionBytes: sessionBytes,
		IndexEntry: map[string]any{
			"id":          fallbackString(transcript.SourceID, "imported-cross-agent"),
			"thread_name": fallbackString(transcript.Title, "Cross-agent handoff"),
			"updated_at":  time.Now().UTC().Format(time.RFC3339Nano),
		},
		ThreadRow: map[string]any{
			"id":                 fallbackString(transcript.SourceID, "imported-cross-agent"),
			"title":              fallbackString(transcript.Title, "Cross-agent handoff"),
			"cwd":                transcript.SourceCWD,
			"source":             "agent-capsule",
			"model_provider":     transcript.SourceAgent,
			"first_user_message": firstUserFromNeutral(transcript),
			"preview":            fallbackString(transcript.Title, "Cross-agent handoff"),
		},
	}
	result, err := codex.RestoreThread(input, opts)
	if err != nil {
		return nil, err
	}
	sidecarDir, writes, err := writeCodexSidecar(result.TargetHome, result.ThreadID, rawSource, transcript, manifestPayload, opts.Execute)
	if err != nil {
		return nil, err
	}
	result.SidecarDir = sidecarDir
	result.Writes = append(result.Writes, writes...)
	return result, nil
}

func codexSessionFromNeutral(transcript neutral.Transcript) ([]byte, error) {
	sourceID := fallbackString(transcript.SourceID, "imported-cross-agent")
	cwd := transcript.SourceCWD
	if cwd == "" {
		cwd = "."
	}
	now := time.Now().UTC()
	var lines []string
	add := func(value map[string]any) error {
		payload, err := json.Marshal(value)
		if err != nil {
			return err
		}
		lines = append(lines, string(payload))
		return nil
	}
	if err := add(map[string]any{
		"timestamp": now.Format(time.RFC3339Nano),
		"type":      "session_meta",
		"payload": map[string]any{
			"id":             sourceID,
			"timestamp":      now.Format(time.RFC3339Nano),
			"cwd":            cwd,
			"cli_version":    "agent-capsule",
			"source":         "agent-capsule",
			"thread_source":  "imported",
			"model_provider": transcript.SourceAgent,
		},
	}); err != nil {
		return nil, err
	}
	if err := add(map[string]any{
		"timestamp": now.Format(time.RFC3339Nano),
		"type":      "turn_context",
		"payload":   map[string]any{"cwd": cwd, "approval_policy": "never"},
	}); err != nil {
		return nil, err
	}
	for i, entry := range transcript.Entries {
		timestamp := now.Add(time.Duration(i+1) * time.Second).Format(time.RFC3339Nano)
		switch entry.Kind {
		case "message":
			role := entry.Role
			if role != "assistant" {
				role = "user"
			}
			contentType := "input_text"
			key := "text"
			if role == "assistant" {
				contentType = "output_text"
			}
			if err := add(map[string]any{
				"timestamp": timestamp,
				"type":      "response_item",
				"payload": map[string]any{
					"type": "message",
					"role": role,
					"content": []map[string]string{
						{"type": contentType, key: entry.Text},
					},
				},
			}); err != nil {
				return nil, err
			}
		case "tool":
			callID := fmt.Sprintf("agent_capsule_tool_%03d", i)
			if err := add(map[string]any{
				"timestamp": timestamp,
				"type":      "response_item",
				"payload": map[string]any{
					"type":      "function_call",
					"name":      fallbackString(entry.Tool, "tool"),
					"namespace": "agent-capsule.source",
					"call_id":   callID,
					"arguments": entry.Input,
					"status":    fallbackString(entry.Status, "called"),
				},
			}); err != nil {
				return nil, err
			}
			if err := add(map[string]any{
				"timestamp": now.Add(time.Duration(i+1)*time.Second + time.Millisecond).Format(time.RFC3339Nano),
				"type":      "response_item",
				"payload": map[string]any{
					"type":    "function_call_output",
					"call_id": callID,
					"output":  entry.Output,
					"status":  fallbackString(entry.Status, "completed"),
				},
			}); err != nil {
				return nil, err
			}
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func writeCodexSidecar(home, threadID string, rawSource []byte, transcript neutral.Transcript, manifestPayload []byte, execute bool) (string, []string, error) {
	home, err := codex.ResolveHome(home)
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(home, "agent-capsule-sources", threadID)
	writes := []string{filepath.Join(dir, "source.jsonl"), filepath.Join(dir, "neutral.json")}
	if len(manifestPayload) > 0 {
		writes = append(writes, filepath.Join(dir, "manifest.json"))
	}
	if !execute {
		return dir, writes, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	if len(rawSource) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "source.jsonl"), rawSource, 0o644); err != nil {
			return "", nil, err
		}
	}
	neutralPayload, err := jsonBytes(transcript)
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "neutral.json"), neutralPayload, 0o644); err != nil {
		return "", nil, err
	}
	if len(manifestPayload) > 0 {
		if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestPayload, 0o644); err != nil {
			return "", nil, err
		}
	}
	return dir, writes, nil
}

func downloadLinkedCapsule(rawURL string) (string, error) {
	manifestURL, key, err := parseLinkKey(rawURL)
	if err != nil {
		return "", err
	}
	manifest, err := fetchLinkManifest(context.Background(), manifestURL)
	if err != nil {
		return "", err
	}
	if err := validateLinkManifest(manifest); err != nil {
		return "", err
	}
	blobURL, err := resolveManifestURL(manifestURL, manifest.Bundle.URL)
	if err != nil {
		return "", err
	}
	ciphertext, err := fetchBytes(context.Background(), blobURL)
	if err != nil {
		return "", err
	}
	if int64(len(ciphertext)) != manifest.Bundle.Bytes {
		return "", fmt.Errorf("ciphertext size mismatch: got %d, want %d", len(ciphertext), manifest.Bundle.Bytes)
	}
	digest := sha256.Sum256(ciphertext)
	if hex.EncodeToString(digest[:]) != manifest.Bundle.SHA256 {
		return "", errors.New("ciphertext sha256 mismatch")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(manifest.Crypto.Nonce)
	if err != nil {
		return "", fmt.Errorf("decode nonce: %w", err)
	}
	plain, err := decryptCapsule(ciphertext, key, nonce)
	if err != nil {
		return "", err
	}
	temp, err := os.CreateTemp("", "capsule-import-*.capsule.zip")
	if err != nil {
		return "", err
	}
	tempPath := temp.Name()
	if _, err := temp.Write(plain); err != nil {
		_ = temp.Close()
		_ = os.Remove(tempPath)
		return "", err
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	return tempPath, nil
}

func detectSourceAgent(opts HandoffOptions) string {
	if os.Getenv("CODEX_THREAD_ID") != "" || os.Getenv("CODEX_SESSION_ID") != "" {
		return AgentCodex
	}
	if os.Getenv("CLAUDE_SESSION_ID") != "" || os.Getenv("CLAUDE_CODE_SESSION_ID") != "" {
		return AgentClaude
	}
	if home, err := codex.ResolveHome(opts.SourceHome); err == nil {
		if _, err := codex.ResolveThreadID(home, opts.SourceThread); err == nil {
			return AgentCodex
		}
	}
	return AgentClaude
}

func resultFields(value any) (status, id, home, cwd string) {
	switch result := value.(type) {
	case *codex.RestoreResult:
		return result.Status, result.ThreadID, result.TargetHome, result.TargetCWD
	case *claude.RestoreResult:
		return result.Status, result.SessionID, result.TargetHome, result.TargetCWD
	default:
		return "ok", "", "", ""
	}
}

func firstUserFromNeutral(transcript neutral.Transcript) string {
	for _, entry := range transcript.Entries {
		if entry.Kind == "message" && entry.Role == "user" && strings.TrimSpace(entry.Text) != "" {
			return entry.Text
		}
	}
	return neutral.HandoffText(transcript, AgentCodex)
}

func normalizeAgent(agent, fallback string) string {
	agent = strings.ToLower(strings.TrimSpace(agent))
	if agent == "" {
		return fallback
	}
	return agent
}

func fallbackString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}
