package profile

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func Fetch(opts FetchOptions) (*FetchResult, error) {
	if strings.TrimSpace(opts.Source) == "" {
		return nil, fmt.Errorf("missing profile source")
	}
	if strings.TrimSpace(opts.Out) == "" {
		return nil, fmt.Errorf("missing --out")
	}
	out, err := filepath.Abs(opts.Out)
	if err != nil {
		return nil, err
	}
	if parsed, parseErr := url.Parse(opts.Source); parseErr == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		return fetchHTTP(opts.Source, out, opts.IncludeGitBundles)
	}
	source, err := filepath.Abs(opts.Source)
	if err != nil {
		return nil, err
	}
	return fetchLocal(source, out, opts.IncludeGitBundles)
}

func fetchLocal(source, out string, includeGitBundles bool) (*FetchResult, error) {
	manifest, err := readManifest(source)
	if err != nil {
		return nil, err
	}
	result := &FetchResult{Status: "ok", BundleDir: out}
	if err := syncBundleFiles(manifest, out, includeGitBundles, func(relative, target string) error {
		sourcePath, err := safeJoin(source, relative)
		if err != nil {
			return err
		}
		info, err := os.Stat(sourcePath)
		if err != nil {
			return err
		}
		return copyFile(sourcePath, target, info.Mode())
	}, result); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(out, "manifest.json"), manifest); err != nil {
		return nil, err
	}
	return result, nil
}

func fetchHTTP(source, out string, includeGitBundles bool) (*FetchResult, error) {
	base := strings.TrimRight(source, "/")
	if strings.HasSuffix(base, "/manifest.json") {
		base = strings.TrimSuffix(base, "/manifest.json")
	}
	client := &http.Client{Timeout: 0}
	response, err := client.Get(base + "/manifest.json")
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch manifest: HTTP %s", response.Status)
	}
	var manifest Manifest
	if err := json.NewDecoder(io.LimitReader(response.Body, 16*1024*1024)).Decode(&manifest); err != nil {
		return nil, err
	}
	if manifest.Schema != Schema {
		return nil, fmt.Errorf("unsupported profile schema %q", manifest.Schema)
	}
	result := &FetchResult{Status: "ok", BundleDir: out}
	if err := syncBundleFiles(&manifest, out, includeGitBundles, func(relative, target string) error {
		segments := strings.Split(filepath.ToSlash(relative), "/")
		for index := range segments {
			segments[index] = url.PathEscape(segments[index])
		}
		requestURL := base + "/" + strings.Join(segments, "/")
		response, err := client.Get(requestURL)
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("fetch %s: HTTP %s", relative, response.Status)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		tmp := target + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
		file, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, response.Body)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		return os.Rename(tmp, target)
	}, result); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(out, "manifest.json"), &manifest); err != nil {
		return nil, err
	}
	return result, nil
}

func syncBundleFiles(manifest *Manifest, out string, includeGitBundles bool, download func(relative, target string) error, result *FetchResult) error {
	type item struct {
		path string
		hash string
		size int64
	}
	items := make([]item, 0, len(manifest.ProfileFiles)+len(manifest.Threads))
	for _, file := range manifest.ProfileFiles {
		if file.LinkTarget != "" {
			continue
		}
		items = append(items, item{path: file.BundlePath, hash: file.SHA256, size: file.Bytes})
	}
	for _, thread := range manifest.Threads {
		items = append(items, item{path: thread.BundlePath, hash: thread.SHA256, size: thread.Bytes})
	}
	if includeGitBundles {
		for _, project := range manifest.Projects {
			for _, repo := range project.Repos {
				if repo.BundlePath != "" {
					items = append(items, item{path: repo.BundlePath, hash: repo.BundleSHA256, size: repo.BundleBytes})
				}
			}
		}
	}
	for _, entry := range items {
		target, err := safeJoin(out, entry.path)
		if err != nil {
			return err
		}
		if fileMatches(target, entry.hash, entry.size) {
			result.ReusedFiles++
			continue
		}
		if err := download(entry.path, target); err != nil {
			return err
		}
		if !fileMatches(target, entry.hash, entry.size) {
			return fmt.Errorf("checksum mismatch after fetching %s", entry.path)
		}
		result.DownloadedFiles++
		result.DownloadedBytes += entry.size
	}
	return nil
}
