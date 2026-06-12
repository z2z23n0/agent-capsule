package capsule

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	PreviewSchema      = "agent-capsule.preview.v1"
	maxPreviewEntries  = 160
	maxPreviewText     = 4000
	maxPreviewToolText = 800
)

type PreviewTranscript struct {
	Schema       string         `json:"schema"`
	ThreadID     string         `json:"thread_id"`
	Title        string         `json:"title"`
	SourceCWD    string         `json:"source_cwd,omitempty"`
	CreatedAt    string         `json:"created_at"`
	MessageCount int            `json:"message_count"`
	ToolCount    int            `json:"tool_count"`
	Truncated    bool           `json:"truncated,omitempty"`
	Entries      []PreviewEntry `json:"entries"`
}

type PreviewEntry struct {
	Kind         string `json:"kind"`
	Timestamp    string `json:"timestamp,omitempty"`
	Role         string `json:"role,omitempty"`
	Text         string `json:"text,omitempty"`
	Tool         string `json:"tool,omitempty"`
	Status       string `json:"status,omitempty"`
	InputPreview string `json:"input_preview,omitempty"`
	OutputBytes  int    `json:"output_bytes,omitempty"`
	Truncated    bool   `json:"truncated,omitempty"`
}

func buildEncryptedPreview(capsulePath string, key []byte) (*LinkPreview, error) {
	loaded, err := load(capsulePath)
	if err != nil {
		return nil, err
	}
	transcript := buildPreviewTranscript(loaded.Manifest, loaded.Session)
	payload, err := jsonBytes(transcript)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, payload, nil)
	return &LinkPreview{
		Schema: PreviewSchema,
		Crypto: LinkCrypto{
			Alg:    CryptoAES256GCM,
			Nonce:  base64.RawURLEncoding.EncodeToString(nonce),
			KeyRef: "url-fragment:k",
		},
		Payload: base64.RawURLEncoding.EncodeToString(ciphertext),
	}, nil
}

func buildPreviewTranscript(manifest Manifest, session []byte) PreviewTranscript {
	transcript := PreviewTranscript{
		Schema:    PreviewSchema,
		ThreadID:  manifest.ThreadID,
		Title:     manifest.ThreadTitle,
		SourceCWD: manifest.SourceCWD,
		CreatedAt: manifest.CreatedAt,
	}
	pendingTools := map[string]int{}
	scanner := bufio.NewScanner(strings.NewReader(string(session)))
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
		timestamp := previewString(item["timestamp"])
		payload, _ := item["payload"].(map[string]any)
		if previewString(item["type"]) != "response_item" {
			continue
		}
		switch previewString(payload["type"]) {
		case "message":
			role := previewString(payload["role"])
			if role != "user" && role != "assistant" {
				continue
			}
			text := previewMessageText(payload["content"])
			if text == "" {
				continue
			}
			if previewHiddenMessage(text) {
				continue
			}
			clipped, truncated := previewClip(text, maxPreviewText)
			if !appendPreviewEntry(&transcript, PreviewEntry{
				Kind:      "message",
				Timestamp: timestamp,
				Role:      role,
				Text:      clipped,
				Truncated: truncated,
			}) {
				continue
			}
			transcript.MessageCount++
		case "function_call", "custom_tool_call", "tool_search_call":
			entry := previewToolCall(timestamp, payload)
			if appendPreviewEntry(&transcript, entry) {
				transcript.ToolCount++
				if callID := previewString(payload["call_id"]); callID != "" {
					pendingTools[callID] = len(transcript.Entries) - 1
				}
			}
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			callID := previewString(payload["call_id"])
			outputBytes := previewOutputBytes(payload["output"])
			if index, ok := pendingTools[callID]; ok && index >= 0 && index < len(transcript.Entries) {
				transcript.Entries[index].OutputBytes = outputBytes
				if transcript.Entries[index].Status == "" {
					transcript.Entries[index].Status = "completed"
				}
				continue
			}
			if appendPreviewEntry(&transcript, PreviewEntry{
				Kind:        "tool",
				Timestamp:   timestamp,
				Tool:        "tool output",
				Status:      "completed",
				OutputBytes: outputBytes,
			}) {
				transcript.ToolCount++
			}
		}
	}
	return transcript
}

func appendPreviewEntry(transcript *PreviewTranscript, entry PreviewEntry) bool {
	if len(transcript.Entries) >= maxPreviewEntries {
		transcript.Truncated = true
		return false
	}
	transcript.Entries = append(transcript.Entries, entry)
	return true
}

func previewToolCall(timestamp string, payload map[string]any) PreviewEntry {
	name := previewString(payload["name"])
	if namespace := previewString(payload["namespace"]); namespace != "" {
		name = namespace + "." + name
	}
	if name == "" {
		name = previewString(payload["type"])
	}
	input := payload["arguments"]
	if input == nil {
		input = payload["input"]
	}
	inputPreview, truncated := previewClip(previewValue(input), maxPreviewToolText)
	status := previewString(payload["status"])
	if status == "" {
		status = "called"
	}
	return PreviewEntry{
		Kind:         "tool",
		Timestamp:    timestamp,
		Tool:         name,
		Status:       status,
		InputPreview: inputPreview,
		Truncated:    truncated,
	}
}

func previewMessageText(content any) string {
	items, ok := content.([]any)
	if !ok {
		return ""
	}
	var parts []string
	for _, item := range items {
		m, _ := item.(map[string]any)
		for _, key := range []string{"text", "output_text"} {
			if text := previewString(m[key]); text != "" {
				parts = append(parts, text)
			}
		}
	}
	return strings.TrimSpace(strings.Join(parts, "\n"))
}

func previewOutputBytes(value any) int {
	if text, ok := value.(string); ok {
		return len([]byte(text))
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return 0
	}
	return len(payload)
}

func previewValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	default:
		payload, err := json.Marshal(v)
		if err != nil {
			return strings.TrimSpace(fmt.Sprint(v))
		}
		return string(payload)
	}
}

func previewString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}

func previewHiddenMessage(text string) bool {
	text = strings.TrimSpace(text)
	return strings.HasPrefix(text, "# AGENTS.md instructions for ") ||
		strings.HasPrefix(text, "<codex_internal_context") ||
		strings.HasPrefix(text, "<environment_context>") ||
		strings.HasPrefix(text, "<INSTRUCTIONS>")
}

func previewClip(text string, max int) (string, bool) {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= max {
		return text, false
	}
	return string(runes[:max]) + "...", true
}
