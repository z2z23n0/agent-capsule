package profile

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var highConfidenceSecrets = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:api[_-]?key|access[_-]?token|secret[_-]?key)\s*[=:]\s*["']?[A-Za-z0-9_./+=-]{20,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`),
	regexp.MustCompile(`\bgh[opsu]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH )?PRIVATE KEY-----`),
}

func resolveHome(path string) (string, error) {
	if path == "" {
		if value := os.Getenv("CODEX_HOME"); value != "" {
			path = value
		} else {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			path = filepath.Join(home, ".codex")
		}
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	return filepath.Abs(path)
}

func safeJoin(root, relative string) (string, error) {
	return safeJoinWithFinalSymlink(root, relative, false)
}

func safeJoinAllowFinalSymlink(root, relative string) (string, error) {
	return safeJoinWithFinalSymlink(root, relative, true)
}

func safeJoinWithFinalSymlink(root, relative string, allowFinalSymlink bool) (string, error) {
	relative = filepath.FromSlash(relative)
	if relative == "" || filepath.IsAbs(relative) {
		return "", fmt.Errorf("unsafe relative path %q", relative)
	}
	clean := filepath.Clean(relative)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("unsafe relative path %q", relative)
	}
	target := filepath.Join(root, clean)
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if isNotExist(err) {
			break
		}
		if err != nil {
			return "", err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			if allowFinalSymlink && index == len(parts)-1 {
				continue
			}
			return "", fmt.Errorf("refusing path through symlink: %s", current)
		}
	}
	return target, nil
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(file)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readManifest(bundleDir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse profile manifest: %w", err)
	}
	if manifest.Schema != Schema {
		return nil, fmt.Errorf("unsupported profile schema %q", manifest.Schema)
	}
	return &manifest, nil
}

func copyAndHash(source, target string, mode fs.FileMode) (string, int64, error) {
	in, err := os.Open(source)
	if err != nil {
		return "", 0, err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", 0, err
	}
	tmp := target + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode.Perm())
	if err != nil {
		return "", 0, err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(out, hash), in)
	closeErr := out.Close()
	if copyErr != nil {
		return "", n, copyErr
	}
	if closeErr != nil {
		return "", n, closeErr
	}
	if err := os.Rename(tmp, target); err != nil {
		return "", n, err
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

func fileHash(path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	hash := sha256.New()
	n, err := io.Copy(hash, file)
	if err != nil {
		return "", n, err
	}
	return hex.EncodeToString(hash.Sum(nil)), n, nil
}

func fileMatches(path, expected string, size int64) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return false
	}
	actual, _, err := fileHash(path)
	return err == nil && actual == expected
}

func copyTreeFiles(sourceRoot, bundleRoot, targetPrefix string, unsafe bool) ([]File, error) {
	var files []File
	err := filepath.WalkDir(sourceRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(sourceRoot, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".system" || strings.HasPrefix(relSlash, ".system/") {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			linkTarget, err := os.Readlink(path)
			if err != nil {
				return err
			}
			targetRel := filepath.ToSlash(filepath.Join(targetPrefix, rel))
			files = append(files, File{TargetRelativePath: targetRel, Mode: uint32(os.ModeSymlink), LinkTarget: linkTarget})
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		targetRel := filepath.ToSlash(filepath.Join(targetPrefix, rel))
		bundleRel := filepath.ToSlash(filepath.Join("profile", targetRel))
		if !unsafe {
			if err := scanFileForSecrets(path); err != nil {
				return err
			}
		}
		bundlePath, err := safeJoin(bundleRoot, bundleRel)
		if err != nil {
			return err
		}
		hash, size, err := copyAndHash(path, bundlePath, info.Mode())
		if err != nil {
			return err
		}
		files = append(files, File{TargetRelativePath: targetRel, BundlePath: bundleRel, SHA256: hash, Bytes: size, Mode: uint32(info.Mode().Perm())})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].TargetRelativePath < files[j].TargetRelativePath })
	return files, err
}

func scanFileForSecrets(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() > 8*1024*1024 {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := scanner.Text()
		for _, pattern := range highConfidenceSecrets {
			if pattern.MatchString(text) {
				return fmt.Errorf("secret scan blocked %s:%d; review it or rerun with --unsafe-include-secrets", path, line)
			}
		}
	}
	return scanner.Err()
}

func copyFile(source, target string, mode fs.FileMode) error {
	_, _, err := copyAndHash(source, target, mode)
	return err
}

func stringValue(value any) string {
	switch item := value.(type) {
	case string:
		return item
	case []byte:
		return string(item)
	case nil:
		return ""
	default:
		return fmt.Sprint(item)
	}
}

func pathWithin(path string, roots []Project) (Project, bool) {
	clean := filepath.Clean(path)
	var match Project
	for _, project := range roots {
		root := filepath.Clean(project.SourcePath)
		if clean == root || strings.HasPrefix(clean, root+string(filepath.Separator)) {
			if len(root) > len(match.SourcePath) {
				match = project
			}
		}
	}
	return match, match.SourcePath != ""
}

func replaceRoot(path, source, target string) string {
	clean := filepath.Clean(path)
	source = filepath.Clean(source)
	if clean == source {
		return target
	}
	if strings.HasPrefix(clean, source+string(filepath.Separator)) {
		return filepath.Join(target, strings.TrimPrefix(clean, source+string(filepath.Separator)))
	}
	return path
}

func quoteShell(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func isNotExist(err error) bool {
	return errors.Is(err, os.ErrNotExist)
}
