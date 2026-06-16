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
	metaTimestamp := fallbackString(transcript.CreatedAt, now.Format(time.RFC3339Nano))
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
		"timestamp": metaTimestamp,
		"type":      "session_meta",
		"payload": map[string]any{
			"id":             sourceID,
			"timestamp":      metaTimestamp,
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
		"timestamp": metaTimestamp,
		"type":      "turn_context",
		"payload":   map[string]any{"cwd": cwd, "approval_policy": "never"},
	}); err != nil {
		return nil, err
	}
	currentTurnID := ""
	responseItemBytes := 0
	lastVisibleTokens := int64(0)
	userTurnCount := 0
	for i, entry := range transcript.Entries {
		timestamp := neutralEntryTimestamp(entry, now, i)
		switch entry.Kind {
		case "message":
			role := entry.Role
			if role == "assistant" && currentTurnID == "" {
				continue
			}
			if role != "assistant" {
				role = "user"
			}
			if role == "user" {
				responseItemBytes += len(entry.Text)
				lastVisibleTokens = approxTokens(responseItemBytes)
				if currentTurnID != "" {
					if err := add(turnCompleteItem(timestamp, currentTurnID)); err != nil {
						return nil, err
					}
				}
				userTurnCount++
				currentTurnID = fmt.Sprintf("agent-capsule-import-turn-%d", userTurnCount)
				if err := add(map[string]any{
					"timestamp": timestamp,
					"type":      "event_msg",
					"payload": map[string]any{
						"type":    "task_started",
						"turn_id": currentTurnID,
					},
				}); err != nil {
					return nil, err
				}
				if err := add(map[string]any{
					"timestamp": timestamp,
					"type":      "event_msg",
					"payload": map[string]any{
						"type":    "user_message",
						"message": entry.Text,
					},
				}); err != nil {
					return nil, err
				}
			} else {
				responseItemBytes += len(entry.Text)
				lastVisibleTokens = approxTokens(responseItemBytes)
				if err := add(map[string]any{
					"timestamp": timestamp,
					"type":      "event_msg",
					"payload": map[string]any{
						"type":    "agent_message",
						"message": entry.Text,
					},
				}); err != nil {
					return nil, err
				}
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
			if currentTurnID == "" {
				continue
			}
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
	if currentTurnID != "" {
		importedAt := now.Add(time.Duration(len(transcript.Entries)+1) * time.Second).Format(time.RFC3339Nano)
		if err := add(map[string]any{
			"timestamp": importedAt,
			"type":      "event_msg",
			"payload": map[string]any{
				"type":    "agent_message",
				"message": "<EXTERNAL SESSION IMPORTED>",
			},
		}); err != nil {
			return nil, err
		}
		if err := add(map[string]any{
			"timestamp": importedAt,
			"type":      "event_msg",
			"payload": map[string]any{
				"type": "token_count",
				"info": map[string]any{
					"total_token_usage": map[string]any{"total_tokens": lastVisibleTokens},
					"last_token_usage":  map[string]any{"total_tokens": lastVisibleTokens},
				},
			},
		}); err != nil {
			return nil, err
		}
		if err := add(turnCompleteItem(importedAt, currentTurnID)); err != nil {
			return nil, err
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), nil
}

func neutralEntryTimestamp(entry neutral.Entry, fallback time.Time, offset int) string {
	if strings.TrimSpace(entry.Timestamp) != "" {
		return entry.Timestamp
	}
	return fallback.Add(time.Duration(offset+1) * time.Second).Format(time.RFC3339Nano)
}

func turnCompleteItem(timestamp, turnID string) map[string]any {
	return map[string]any{
		"timestamp": timestamp,
		"type":      "event_msg",
		"payload": map[string]any{
			"type":    "task_complete",
			"turn_id": turnID,
		},
	}
}

func approxTokens(bytes int) int64 {
	if bytes <= 0 {
		return 0
	}
	tokens := int64((bytes + 3) / 4)
	if tokens < 1 {
		return 1
	}
	return tokens
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
