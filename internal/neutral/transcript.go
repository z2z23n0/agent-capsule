package neutral

import (
	"bufio"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/z2z23n0/agent-capsule/internal/codex"
)

const Schema = "agent-capsule.neutral.v1"

const (
	externalAgentToolCallTag   = "external_agent_tool_call"
	externalAgentToolResultTag = "external_agent_tool_result"
	toolCallNoteMaxLen         = 2000
	toolResultNoteMaxLen       = 4000
)

type Transcript struct {
	Schema      string  `json:"schema"`
	SourceAgent string  `json:"source_agent"`
	SourceID    string  `json:"source_id"`
	Title       string  `json:"title"`
	SourceCWD   string  `json:"source_cwd,omitempty"`
	CreatedAt   string  `json:"created_at"`
	Entries     []Entry `json:"entries"`
}

type Entry struct {
	Kind      string `json:"kind"`
	Timestamp string `json:"timestamp,omitempty"`
	Role      string `json:"role,omitempty"`
	Text      string `json:"text,omitempty"`
	Tool      string `json:"tool,omitempty"`
	Status    string `json:"status,omitempty"`
	Input     string `json:"input,omitempty"`
	Output    string `json:"output,omitempty"`
}

func FromCodexSession(sourceID, title, sourceCWD string, session []byte) Transcript {
	if normalized, err := codex.NormalizeActiveSession(session, sourceID); err == nil {
		session = normalized
	}
	out := Transcript{
		Schema:      Schema,
		SourceAgent: "codex",
		SourceID:    sourceID,
		Title:       title,
		SourceCWD:   sourceCWD,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
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
		timestamp := stringValue(item["timestamp"])
		payload, _ := item["payload"].(map[string]any)
		switch stringValue(item["type"]) {
		case "session_meta":
			if out.SourceID == "" {
				out.SourceID = stringValue(payload["id"])
			}
			if out.SourceCWD == "" {
				out.SourceCWD = stringValue(payload["cwd"])
			}
		case "turn_context":
			if out.SourceCWD == "" {
				out.SourceCWD = stringValue(payload["cwd"])
			}
		case "event_msg":
			if stringValue(payload["type"]) == "agent_message" {
				text := strings.TrimSpace(stringValue(payload["message"]))
				if text != "" {
					out.Entries = append(out.Entries, Entry{Kind: "message", Timestamp: timestamp, Role: "assistant", Text: text})
				}
			}
		case "response_item":
			switch stringValue(payload["type"]) {
			case "message":
				role := stringValue(payload["role"])
				if role != "user" && role != "assistant" {
					continue
				}
				text := extractCodexMessageText(payload["content"])
				if text == "" || hiddenMessage(text) {
					continue
				}
				out.Entries = append(out.Entries, Entry{Kind: "message", Timestamp: timestamp, Role: role, Text: text})
			case "function_call", "custom_tool_call", "tool_search_call":
				entry := Entry{
					Kind:      "tool",
					Timestamp: timestamp,
					Tool:      toolName(payload),
					Status:    fallbackString(stringValue(payload["status"]), "called"),
					Input:     previewValue(firstNonNil(payload["arguments"], payload["input"])),
				}
				out.Entries = append(out.Entries, entry)
				if callID := stringValue(payload["call_id"]); callID != "" {
					pendingTools[callID] = len(out.Entries) - 1
				}
			case "function_call_output", "custom_tool_call_output", "tool_search_output":
				callID := stringValue(payload["call_id"])
				output := previewValue(payload["output"])
				status := fallbackString(stringValue(payload["status"]), "completed")
				if index, ok := pendingTools[callID]; ok && index >= 0 && index < len(out.Entries) {
					out.Entries[index].Output = output
					out.Entries[index].Status = status
					continue
				}
				out.Entries = append(out.Entries, Entry{Kind: "tool", Timestamp: timestamp, Tool: "tool output", Status: status, Output: output})
			}
		}
	}
	return out
}

func FromClaudeSession(sourceID, title, sourceCWD string, session []byte) Transcript {
	out := Transcript{
		Schema:      Schema,
		SourceAgent: "claude",
		SourceID:    sourceID,
		Title:       title,
		SourceCWD:   sourceCWD,
		CreatedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
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
		if out.SourceID == "" {
			out.SourceID = stringValue(item["sessionId"])
		}
		if out.SourceCWD == "" {
			out.SourceCWD = stringValue(item["cwd"])
		}
		if title := titleFromClaudeRecord(item, "custom-title", "customTitle"); title != "" {
			out.Title = title
			continue
		}
		if out.Title == "" {
			out.Title = titleFromClaudeRecord(item, "ai-title", "aiTitle")
		}
		if boolValue(item["isMeta"]) || boolValue(item["isSidechain"]) {
			continue
		}
		timestamp := stringValue(item["timestamp"])
		switch stringValue(item["type"]) {
		case "user", "assistant":
			message, _ := item["message"].(map[string]any)
			role := stringValue(message["role"])
			if role == "" {
				role = stringValue(item["type"])
			}
			text, onlyToolResult := extractClaudeMessage(message["content"])
			if text == "" || hiddenMessage(text) {
				continue
			}
			if role == "assistant" || onlyToolResult {
				role = "assistant"
			} else {
				role = "user"
			}
			out.Entries = append(out.Entries, Entry{Kind: "message", Timestamp: timestamp, Role: role, Text: text})
		case "attachment", "file-history-snapshot":
			text := previewValue(firstNonNil(item["attachment"], item["snapshot"]))
			if text != "" {
				out.Entries = append(out.Entries, Entry{Kind: "tool", Timestamp: timestamp, Tool: stringValue(item["type"]), Status: "recorded", Output: text})
			}
		}
	}
	return out
}

func HandoffText(t Transcript, targetAgent string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Agent Capsule cross-agent handoff\n\n")
	fmt.Fprintf(&b, "Source agent: %s\n", t.SourceAgent)
	fmt.Fprintf(&b, "Source session: %s\n", t.SourceID)
	if t.Title != "" {
		fmt.Fprintf(&b, "Title: %s\n", t.Title)
	}
	if t.SourceCWD != "" {
		fmt.Fprintf(&b, "Source cwd: %s\n", t.SourceCWD)
	}
	fmt.Fprintf(&b, "Target agent: %s\n", targetAgent)
	fmt.Fprintf(&b, "\nContinue from the transcript below. Raw source session data is stored locally as an Agent Capsule sidecar for audit and deeper recovery.\n\n")
	fmt.Fprintf(&b, "Transcript:\n")
	for _, entry := range t.Entries {
		switch entry.Kind {
		case "message":
			fmt.Fprintf(&b, "\n[%s] %s\n", entry.Role, entry.Text)
		case "tool":
			fmt.Fprintf(&b, "\n[tool] %s", entry.Tool)
			if entry.Status != "" {
				fmt.Fprintf(&b, " (%s)", entry.Status)
			}
			if entry.Input != "" {
				fmt.Fprintf(&b, "\ninput: %s", entry.Input)
			}
			if entry.Output != "" {
				fmt.Fprintf(&b, "\noutput: %s", entry.Output)
			}
			fmt.Fprintf(&b, "\n")
		}
	}
	return strings.TrimSpace(b.String())
}

func extractCodexMessageText(content any) string {
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

func extractClaudeMessage(content any) (string, bool) {
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value), false
	case []any:
		var parts []string
		onlyToolResult := len(value) > 0
		for _, item := range value {
			m, _ := item.(map[string]any)
			switch stringValue(m["type"]) {
			case "text":
				if text := stringValue(m["text"]); text != "" {
					parts = append(parts, text)
					onlyToolResult = false
				}
			case "tool_use":
				parts = append(parts, claudeToolCallNote(m))
				onlyToolResult = false
			case "tool_result":
				parts = append(parts, claudeToolResultNote(m))
			case "thinking":
			case "":
			default:
				parts = append(parts, "[external unsupported block: "+stringValue(m["type"])+"]")
				onlyToolResult = false
			}
		}
		return strings.TrimSpace(strings.Join(parts, "\n\n")), onlyToolResult
	default:
		return "", false
	}
}

func claudeToolCallNote(block map[string]any) string {
	name := fallbackString(stringValue(block["name"]), "unknown")
	lines := []string{fmt.Sprintf("[%s: %s]", externalAgentToolCallTag, name)}
	if input, ok := block["input"].(map[string]any); ok {
		if description := stringValue(input["description"]); description != "" {
			lines = append(lines, "description: "+description)
		}
		if command := stringValue(input["command"]); command != "" {
			lines = append(lines, "command: "+command)
		}
		if file := stringValue(firstNonNil(input["file_path"], input["file"])); file != "" {
			lines = append(lines, "file: "+file)
		}
		if len(lines) == 1 {
			lines = append(lines, "input: "+clip(previewValue(input), toolCallNoteMaxLen))
		}
	} else if input := previewValue(block["input"]); input != "" {
		lines = append(lines, "input: "+clip(input, toolCallNoteMaxLen))
	}
	lines = append(lines, fmt.Sprintf("[/%s]", externalAgentToolCallTag))
	return strings.Join(lines, "\n")
}

func claudeToolResultNote(block map[string]any) string {
	label := "[" + externalAgentToolResultTag + "]"
	if boolValue(block["is_error"]) {
		label = "[" + externalAgentToolResultTag + ": error]"
	}
	text := claudeToolResultText(block["content"])
	if text == "" {
		return label + "\n[/" + externalAgentToolResultTag + "]"
	}
	return label + "\n" + clip(text, toolResultNoteMaxLen) + "\n[/" + externalAgentToolResultTag + "]"
}

func claudeToolResultText(content any) string {
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

func toolName(payload map[string]any) string {
	name := stringValue(payload["name"])
	if namespace := stringValue(payload["namespace"]); namespace != "" {
		name = namespace + "." + name
	}
	if name == "" {
		name = stringValue(payload["type"])
	}
	return name
}

func previewValue(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return strings.TrimSpace(string(payload))
}

func hiddenMessage(text string) bool {
	text = strings.TrimSpace(text)
	for _, prefix := range []string{
		"# AGENTS.md instructions for ",
		"<environment_context>",
		"<skill>",
		"<turn_aborted>",
		"<INSTRUCTIONS>",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func fallbackString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func titleFromClaudeRecord(record map[string]any, recordType, field string) string {
	if stringValue(record["type"]) != recordType {
		return ""
	}
	return strings.TrimSpace(stringValue(record[field]))
}

func clip(text string, max int) string {
	if max <= 0 || len([]rune(text)) <= max {
		return text
	}
	runes := []rune(text)
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

func stringValue(value any) string {
	switch v := value.(type) {
	case string:
		return v
	default:
		return ""
	}
}

func boolValue(value any) bool {
	v, _ := value.(bool)
	return v
}
