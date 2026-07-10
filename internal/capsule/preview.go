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

	"github.com/z2z23n0/agent-capsule/internal/codex"
)

const (
	PreviewSchema             = "agent-capsule.preview.v1"
	maxPreviewEntries         = 160
	maxPreviewText            = 4000
	maxPreviewToolText        = 800
	maxPreviewImages          = 12
	maxPreviewImagePayloadLen = 1536 * 1024
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
	Kind          string         `json:"kind"`
	Timestamp     string         `json:"timestamp,omitempty"`
	Role          string         `json:"role,omitempty"`
	Text          string         `json:"text,omitempty"`
	Images        []PreviewImage `json:"images,omitempty"`
	OmittedImages int            `json:"omitted_images,omitempty"`
	Skills        []PreviewSkill `json:"skills,omitempty"`
	Tool          string         `json:"tool,omitempty"`
	Status        string         `json:"status,omitempty"`
	InputPreview  string         `json:"input_preview,omitempty"`
	Output        string         `json:"output,omitempty"`
	OutputBytes   int            `json:"output_bytes,omitempty"`
	DurationMS    int64          `json:"duration_ms,omitempty"`
	Truncated     bool           `json:"truncated,omitempty"`
}

type PreviewSkill struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
	Text string `json:"text,omitempty"`
}

type PreviewImage struct {
	Src     string `json:"src,omitempty"`
	MIME    string `json:"mime,omitempty"`
	Bytes   int    `json:"bytes,omitempty"`
	Alt     string `json:"alt,omitempty"`
	Omitted bool   `json:"omitted,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func buildEncryptedPreview(capsulePath string, key []byte) (*LinkPreview, error) {
	loaded, err := load(capsulePath)
	if err != nil {
		return nil, err
	}
	transcript := buildPreviewTranscript(loaded.Manifest, loaded.Session, loaded.ImageAssets...)
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

func buildPreviewTranscript(manifest Manifest, session []byte, assets ...imageAssetFile) PreviewTranscript {
	if normalized, err := codex.NormalizeActiveSession(session, manifest.ThreadID); err == nil {
		session = normalized
	}
	transcript := PreviewTranscript{
		Schema:    PreviewSchema,
		ThreadID:  manifest.ThreadID,
		Title:     manifest.ThreadTitle,
		SourceCWD: manifest.SourceCWD,
		CreatedAt: manifest.CreatedAt,
	}
	pendingTools := map[string]int{}
	turnStart := -1
	imageState := &previewImageState{
		Assets:        previewImageAssets(assets),
		MaxImages:     maxPreviewImages,
		MaxPayloadLen: maxPreviewImagePayloadLen,
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
		timestamp := previewString(item["timestamp"])
		payload, _ := item["payload"].(map[string]any)
		switch previewString(item["type"]) {
		case "event_msg":
			switch previewString(payload["type"]) {
			case "task_started":
				turnStart = len(transcript.Entries)
			case "task_complete":
				markPreviewTurnDuration(&transcript, turnStart, previewInt64(payload["duration_ms"]))
				turnStart = -1
			}
			continue
		case "response_item":
		default:
			continue
		}
		switch previewString(payload["type"]) {
		case "message":
			role := previewString(payload["role"])
			if role != "user" && role != "assistant" {
				continue
			}
			rawText, text, images, omitted := previewMessageContent(payload["content"], imageState)
			if rawText == "" && len(images) == 0 && omitted == 0 {
				continue
			}
			if skill, ok := previewSkillMessage(rawText); ok {
				attachPreviewSkill(&transcript, skill)
				continue
			}
			if text == "" && len(images) == 0 && omitted == 0 {
				continue
			}
			clipped, truncated := previewClip(text, maxPreviewText)
			if !appendPreviewEntry(&transcript, PreviewEntry{
				Kind:          "message",
				Timestamp:     timestamp,
				Role:          role,
				Text:          clipped,
				Images:        images,
				OmittedImages: omitted,
				Truncated:     truncated,
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
			output := previewOutputText(payload["output"])
			outputBytes := len([]byte(output))
			status := previewString(payload["status"])
			if status == "" {
				status = "completed"
			}
			if index, ok := pendingTools[callID]; ok && index >= 0 && index < len(transcript.Entries) {
				transcript.Entries[index].Output = output
				transcript.Entries[index].OutputBytes = outputBytes
				if transcript.Entries[index].Status == "" {
					transcript.Entries[index].Status = status
				}
				continue
			}
			if appendPreviewEntry(&transcript, PreviewEntry{
				Kind:        "tool",
				Timestamp:   timestamp,
				Tool:        "tool output",
				Status:      status,
				Output:      output,
				OutputBytes: outputBytes,
			}) {
				transcript.ToolCount++
			}
		}
	}
	return transcript
}

func markPreviewTurnDuration(transcript *PreviewTranscript, start int, durationMS int64) {
	if durationMS <= 0 || start < 0 || start >= len(transcript.Entries) {
		return
	}
	for i := start; i < len(transcript.Entries); i++ {
		transcript.Entries[i].DurationMS = durationMS
	}
}

func attachPreviewSkill(transcript *PreviewTranscript, skill PreviewSkill) {
	for i := len(transcript.Entries) - 1; i >= 0; i-- {
		entry := &transcript.Entries[i]
		if entry.Kind == "message" && entry.Role == "user" {
			entry.Skills = append(entry.Skills, skill)
			return
		}
	}
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
	inputPreview, truncated := previewClip(previewRedactHiddenContext(previewValue(input)), maxPreviewToolText)
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

type previewImageState struct {
	Assets        map[string]PreviewImage
	Seen          map[string]bool
	Count         int
	PayloadLen    int
	MaxImages     int
	MaxPayloadLen int
}

func previewMessageContent(content any, state *previewImageState) (string, string, []PreviewImage, int) {
	items, ok := content.([]any)
	if !ok {
		return "", "", nil, 0
	}
	var rawParts []string
	var visibleParts []string
	var tagPaths []string
	var inputImages []PreviewImage
	var images []PreviewImage
	omitted := 0
	for _, item := range items {
		m, _ := item.(map[string]any)
		for _, key := range []string{"text", "output_text"} {
			if text := previewString(m[key]); text != "" {
				rawParts = append(rawParts, text)
				if visible := previewRedactMessage(text); visible != "" {
					visibleParts = append(visibleParts, visible)
					tagPaths = append(tagPaths, imageTagPaths(visible)...)
				}
			}
		}
		if previewString(m["type"]) == "input_image" {
			image, ok := previewDataImage(previewString(m["image_url"]), previewString(m["detail"]))
			if !ok {
				continue
			}
			inputImages = append(inputImages, image)
		}
	}
	if len(inputImages) > 0 {
		for _, image := range inputImages {
			if added, limited := addPreviewImage(state, image); added {
				images = append(images, image)
			} else if limited {
				omitted++
			}
		}
	} else {
		for _, path := range tagPaths {
			image, ok := state.Assets[path]
			if !ok {
				continue
			}
			if added, limited := addPreviewImage(state, image); added {
				images = append(images, image)
			} else if limited {
				omitted++
			}
		}
	}
	return strings.TrimSpace(strings.Join(rawParts, "\n")), strings.TrimSpace(strings.Join(visibleParts, "\n")), images, omitted
}

func previewImageAssets(assets []imageAssetFile) map[string]PreviewImage {
	out := map[string]PreviewImage{}
	for _, asset := range assets {
		if asset.Metadata.SourcePath == "" || len(asset.Content) == 0 || asset.Metadata.MIME == "" {
			continue
		}
		src := "data:" + asset.Metadata.MIME + ";base64," + base64.StdEncoding.EncodeToString(asset.Content)
		out[asset.Metadata.SourcePath] = PreviewImage{
			Src:   src,
			MIME:  asset.Metadata.MIME,
			Bytes: len(asset.Content),
			Alt:   asset.Metadata.OriginalName,
		}
	}
	return out
}

func previewDataImage(src, detail string) (PreviewImage, bool) {
	if !strings.HasPrefix(src, "data:image/") {
		return PreviewImage{}, false
	}
	mime := "image"
	if comma := strings.IndexByte(src, ','); comma > 5 {
		media := src[len("data:"):comma]
		if semi := strings.IndexByte(media, ';'); semi >= 0 {
			media = media[:semi]
		}
		if media != "" {
			mime = media
		}
	}
	alt := "uploaded image"
	if detail != "" {
		alt += " (" + detail + ")"
	}
	return PreviewImage{
		Src:   src,
		MIME:  mime,
		Bytes: int(dataURLDecodedBytes(src)),
		Alt:   alt,
	}, true
}

func addPreviewImage(state *previewImageState, image PreviewImage) (bool, bool) {
	if state == nil {
		return false, false
	}
	if state.Seen == nil {
		state.Seen = map[string]bool{}
	}
	key := image.Src
	if key == "" {
		key = image.Alt
	}
	if key != "" && state.Seen[key] {
		return false, false
	}
	payloadLen := len(image.Src)
	if state.Count >= state.MaxImages || state.PayloadLen+payloadLen > state.MaxPayloadLen {
		return false, true
	}
	if key != "" {
		state.Seen[key] = true
	}
	state.Count++
	state.PayloadLen += payloadLen
	return true, false
}

func previewOutputText(value any) string {
	if value == nil {
		return ""
	}
	var text string
	if text, ok := value.(string); ok {
		return previewRedactHiddenContext(text)
	}
	payload, err := json.Marshal(value)
	if err != nil {
		text = strings.TrimSpace(fmt.Sprint(value))
	} else {
		text = string(payload)
	}
	return previewRedactHiddenContext(text)
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

func previewInt64(value any) int64 {
	switch v := value.(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

func previewRedactMessage(text string) string {
	text = strings.TrimSpace(previewRedactHiddenContext(text))
	if text == "[internal context omitted]" {
		return ""
	}
	return text
}

func previewRedactHiddenContext(text string) string {
	if text == "" {
		return ""
	}
	var b strings.Builder
	inHiddenBlock := false
	redactedPrevious := false
	for _, part := range strings.SplitAfter(text, "\n") {
		line := strings.TrimRight(part, "\r\n")
		trimmed := strings.TrimSpace(line)
		if inHiddenBlock {
			if !redactedPrevious {
				b.WriteString("[internal context omitted]\n")
				redactedPrevious = true
			}
			if previewEndsHiddenContext(trimmed) {
				inHiddenBlock = false
			}
			continue
		}
		if previewContainsHiddenContext(trimmed) {
			if !redactedPrevious {
				b.WriteString("[internal context omitted]\n")
				redactedPrevious = true
			}
			if previewStartsHiddenContext(trimmed) && !previewEndsHiddenContext(trimmed) {
				inHiddenBlock = true
			}
			continue
		}
		b.WriteString(part)
		redactedPrevious = false
	}
	return b.String()
}

func previewContainsHiddenContext(text string) bool {
	if previewStartsHiddenContext(text) {
		return true
	}
	for _, marker := range []string{
		"# AGENTS.md instructions",
		"<recommended_plugins>",
		"<codex_internal_context",
		"<environment_context>",
		"<INSTRUCTIONS>",
		"<skill>",
	} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func previewStartsHiddenContext(text string) bool {
	for _, prefix := range []string{
		"# AGENTS.md instructions",
		"<recommended_plugins>",
		"<codex_internal_context",
		"<environment_context>",
		"<INSTRUCTIONS>",
		"<skill>",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func previewEndsHiddenContext(text string) bool {
	for _, prefix := range []string{
		"</recommended_plugins>",
		"</codex_internal_context>",
		"</environment_context>",
		"</INSTRUCTIONS>",
		"</skill>",
	} {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func previewClip(text string, max int) (string, bool) {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) <= max {
		return text, false
	}
	return string(runes[:max]) + "...", true
}

func previewSkillMessage(text string) (PreviewSkill, bool) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "<skill>") {
		return PreviewSkill{}, false
	}
	name := previewXMLTag(text, "name")
	if name == "" {
		name = "skill"
	}
	return PreviewSkill{
		Name: name,
		Path: previewXMLTag(text, "path"),
		Text: text,
	}, true
}

func previewXMLTag(text, tag string) string {
	open := "<" + tag + ">"
	close := "</" + tag + ">"
	start := strings.Index(text, open)
	if start < 0 {
		return ""
	}
	start += len(open)
	end := strings.Index(text[start:], close)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}
