package capsule

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/z2z23n0/agent-capsule/internal/codex"
)

const (
	ImageAssetsSchema       = "agent-capsule.images.v1"
	ImageAssetsManifestPath = "codex/assets/images.json"
	imageAssetsDir          = "codex/assets/images"
)

var imageTagPathPattern = regexp.MustCompile(`<image\b[^>]*\bpath=(?:"([^"]+)"|'([^']+)')[^>]*>`)

type ImageSummary struct {
	Embedded      int      `json:"embedded"`
	EmbeddedBytes int64    `json:"embedded_bytes,omitempty"`
	Copied        int      `json:"copied"`
	Bytes         int64    `json:"bytes"`
	Missing       int      `json:"missing"`
	Unsupported   int      `json:"unsupported"`
	Warnings      []string `json:"warnings,omitempty"`
}

type ImageAssetsManifest struct {
	Schema   string               `json:"schema"`
	Summary  ImageSummary         `json:"summary"`
	Images   []ImageAssetMetadata `json:"images"`
	Warnings []string             `json:"warnings,omitempty"`
}

type ImageAssetMetadata struct {
	ID           string `json:"id"`
	SourcePath   string `json:"source_path"`
	ZipPath      string `json:"zip_path,omitempty"`
	SHA256       string `json:"sha256,omitempty"`
	MIME         string `json:"mime,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	OriginalName string `json:"original_name,omitempty"`
	Status       string `json:"status"`
	Reason       string `json:"reason,omitempty"`
}

type imageBundle struct {
	Manifest ImageAssetsManifest
	Files    map[string][]byte
}

type imageAssetFile struct {
	Metadata ImageAssetMetadata
	Content  []byte
}

type sessionImageRefs struct {
	Embedded      int
	EmbeddedBytes int64
	LocalPaths    []string
}

func buildImageBundle(data *codex.ExportData) imageBundle {
	refs := collectSessionImageRefs(data.SessionBytes)
	bundle := imageBundle{
		Manifest: ImageAssetsManifest{
			Schema: ImageAssetsSchema,
			Summary: ImageSummary{
				Embedded:      refs.Embedded,
				EmbeddedBytes: refs.EmbeddedBytes,
			},
		},
		Files: map[string][]byte{},
	}
	seenSource := map[string]bool{}
	seenSHA := map[string]bool{}
	for _, sourcePath := range refs.LocalPaths {
		if sourcePath == "" || seenSource[sourcePath] {
			continue
		}
		seenSource[sourcePath] = true
		metadata, content := packageImageAsset(sourcePath, data.SourceCWD)
		bundle.Manifest.Images = append(bundle.Manifest.Images, metadata)
		switch metadata.Status {
		case "copied":
			bundle.Manifest.Summary.Copied++
			if !seenSHA[metadata.SHA256] {
				seenSHA[metadata.SHA256] = true
				bundle.Manifest.Summary.Bytes += metadata.Bytes
				bundle.Files[metadata.ZipPath] = content
			}
		case "missing":
			bundle.Manifest.Summary.Missing++
			bundle.Manifest.Summary.Warnings = append(bundle.Manifest.Summary.Warnings, fmt.Sprintf("missing image %s: %s", sourcePath, metadata.Reason))
		case "unsupported":
			bundle.Manifest.Summary.Unsupported++
			bundle.Manifest.Summary.Warnings = append(bundle.Manifest.Summary.Warnings, fmt.Sprintf("unsupported image %s: %s", sourcePath, metadata.Reason))
		}
	}
	bundle.Manifest.Warnings = bundle.Manifest.Summary.Warnings
	return bundle
}

func (b imageBundle) hasManifest() bool {
	return len(b.Manifest.Images) > 0
}

func (b imageBundle) summary() ImageSummary {
	return b.Manifest.Summary
}

func imageFiles(bundle imageBundle) []string {
	var names []string
	if bundle.hasManifest() {
		names = append(names, ImageAssetsManifestPath)
	}
	for name := range bundle.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func packageImageAsset(sourcePath, sourceCWD string) (ImageAssetMetadata, []byte) {
	resolved := resolveSourceImagePath(sourcePath, sourceCWD)
	metadata := ImageAssetMetadata{
		ID:           imageID(sourcePath),
		SourcePath:   sourcePath,
		OriginalName: filepath.Base(sourcePath),
	}
	content, err := os.ReadFile(resolved)
	if err != nil {
		metadata.Status = "missing"
		metadata.Reason = err.Error()
		return metadata, nil
	}
	mime := detectImageMIME(resolved, content)
	if mime == "" {
		metadata.Status = "unsupported"
		metadata.Reason = "not a supported image type"
		return metadata, nil
	}
	digest := sha256.Sum256(content)
	sha := hex.EncodeToString(digest[:])
	ext := imageExt(resolved, mime)
	metadata.ID = "img_" + sha[:16]
	metadata.ZipPath = imageAssetsDir + "/" + sha + ext
	metadata.SHA256 = sha
	metadata.MIME = mime
	metadata.Bytes = int64(len(content))
	metadata.Status = "copied"
	return metadata, content
}

func collectSessionImageRefs(session []byte) sessionImageRefs {
	var refs sessionImageRefs
	scanner := bufio.NewScanner(strings.NewReader(string(session)))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	seenLocal := map[string]bool{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item any
		if json.Unmarshal([]byte(line), &item) != nil {
			continue
		}
		collectImageRefs(item, &refs, seenLocal)
	}
	return refs
}

func collectImageRefs(value any, refs *sessionImageRefs, seenLocal map[string]bool) {
	switch v := value.(type) {
	case map[string]any:
		if imageString(v["type"]) == "input_image" {
			if imageURL := imageString(v["image_url"]); strings.HasPrefix(imageURL, "data:image/") {
				refs.Embedded++
				refs.EmbeddedBytes += dataURLDecodedBytes(imageURL)
			}
		}
		if images, ok := v["local_images"].([]any); ok {
			for _, image := range images {
				appendLocalImageRef(imageString(image), refs, seenLocal)
			}
		}
		for _, child := range v {
			collectImageRefs(child, refs, seenLocal)
		}
	case []any:
		for _, child := range v {
			collectImageRefs(child, refs, seenLocal)
		}
	case string:
		for _, sourcePath := range imageTagPaths(v) {
			appendLocalImageRef(sourcePath, refs, seenLocal)
		}
	}
}

func appendLocalImageRef(sourcePath string, refs *sessionImageRefs, seen map[string]bool) {
	sourcePath = strings.TrimSpace(sourcePath)
	if sourcePath == "" || seen[sourcePath] {
		return
	}
	seen[sourcePath] = true
	refs.LocalPaths = append(refs.LocalPaths, sourcePath)
}

func imageTagPaths(text string) []string {
	var paths []string
	for _, match := range imageTagPathPattern.FindAllStringSubmatch(text, -1) {
		if match[1] != "" {
			paths = append(paths, match[1])
			continue
		}
		if match[2] != "" {
			paths = append(paths, match[2])
		}
	}
	return paths
}

func loadImageAssets(payloads map[string][]byte) ([]imageAssetFile, ImageSummary, error) {
	manifestPayload, ok := payloads[ImageAssetsManifestPath]
	if !ok {
		return nil, ImageSummary{}, nil
	}
	var manifest ImageAssetsManifest
	if err := json.Unmarshal(manifestPayload, &manifest); err != nil {
		return nil, ImageSummary{}, err
	}
	if manifest.Schema != ImageAssetsSchema {
		return nil, ImageSummary{}, fmt.Errorf("unsupported image assets schema %q", manifest.Schema)
	}
	var assets []imageAssetFile
	for _, metadata := range manifest.Images {
		if metadata.Status != "copied" {
			continue
		}
		content, ok := payloads[metadata.ZipPath]
		if !ok {
			return nil, ImageSummary{}, fmt.Errorf("image asset missing %s", metadata.ZipPath)
		}
		digest := sha256.Sum256(content)
		if hex.EncodeToString(digest[:]) != metadata.SHA256 {
			return nil, ImageSummary{}, fmt.Errorf("image asset sha256 mismatch for %s", metadata.ZipPath)
		}
		assets = append(assets, imageAssetFile{Metadata: metadata, Content: content})
	}
	return assets, manifest.Summary, nil
}

func restoreImageAssets(assets []imageAssetFile) []codex.RestoreImageAsset {
	out := make([]codex.RestoreImageAsset, 0, len(assets))
	for _, asset := range assets {
		out = append(out, codex.RestoreImageAsset{
			SourcePath: asset.Metadata.SourcePath,
			FileName:   filepath.Base(asset.Metadata.ZipPath),
			SHA256:     asset.Metadata.SHA256,
			MIME:       asset.Metadata.MIME,
			Bytes:      asset.Metadata.Bytes,
			Content:    asset.Content,
		})
	}
	return out
}

func summarizeLoadedImages(session []byte, stored ImageSummary) ImageSummary {
	refs := collectSessionImageRefs(session)
	stored.Embedded = refs.Embedded
	stored.EmbeddedBytes = refs.EmbeddedBytes
	return stored
}

func resolveSourceImagePath(sourcePath, sourceCWD string) string {
	if filepath.IsAbs(sourcePath) || sourceCWD == "" {
		return sourcePath
	}
	return filepath.Join(sourceCWD, sourcePath)
}

func detectImageMIME(path string, content []byte) string {
	if len(content) == 0 {
		return ""
	}
	limit := len(content)
	if limit > 512 {
		limit = 512
	}
	detected := http.DetectContentType(content[:limit])
	if strings.HasPrefix(detected, "image/") {
		return detected
	}
	if strings.EqualFold(filepath.Ext(path), ".webp") && isWebP(content) {
		return "image/webp"
	}
	return ""
}

func imageExt(path, mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		ext := strings.ToLower(filepath.Ext(path))
		if ext != "" {
			return ext
		}
		return ".img"
	}
}

func isWebP(content []byte) bool {
	return len(content) >= 12 &&
		string(content[:4]) == "RIFF" &&
		string(content[8:12]) == "WEBP"
}

func imageID(sourcePath string) string {
	digest := sha256.Sum256([]byte(sourcePath))
	return "img_" + hex.EncodeToString(digest[:8])
}

func dataURLDecodedBytes(value string) int64 {
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return int64(len(value))
	}
	data := strings.TrimSpace(value[comma+1:])
	padding := strings.Count(strings.TrimRight(data, "="), "=")
	if strings.HasSuffix(data, "==") {
		padding = 2
	} else if strings.HasSuffix(data, "=") {
		padding = 1
	}
	return int64((len(data)*3)/4 - padding)
}

func imageString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}
