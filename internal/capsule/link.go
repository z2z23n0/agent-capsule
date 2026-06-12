package capsule

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/z2z23n0/agent-capsule/internal/codex"
)

const (
	LinkSchema       = "agent-capsule.link.v1"
	CryptoAES256GCM  = "AES-256-GCM"
	DefaultShareMode = "official"
	ZipShareFormat   = "zip"
	LinkShareFormat  = "link"
)

type LinkManifest struct {
	Schema    string          `json:"schema"`
	CreatedAt string          `json:"created_at"`
	ExpiresAt string          `json:"expires_at,omitempty"`
	Thread    LinkThread      `json:"thread"`
	Bundle    LinkBundle      `json:"bundle"`
	Crypto    LinkCrypto      `json:"crypto"`
	Import    LinkImport      `json:"import"`
	Preview   *LinkPreview    `json:"preview,omitempty"`
	Service   LinkServiceInfo `json:"service,omitempty"`
}

type LinkThread struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

type LinkBundle struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type LinkCrypto struct {
	Alg    string `json:"alg"`
	Nonce  string `json:"nonce"`
	KeyRef string `json:"key_ref"`
}

type LinkImport struct {
	Tool           string `json:"tool"`
	Command        string `json:"command"`
	InstallCommand string `json:"install_command,omitempty"`
	DryRunCommand  string `json:"dry_run_command,omitempty"`
	ExecuteCommand string `json:"execute_command,omitempty"`
	DocsURL        string `json:"docs_url,omitempty"`
}

type LinkPreview struct {
	Schema  string     `json:"schema"`
	Crypto  LinkCrypto `json:"crypto"`
	Payload string     `json:"payload"`
}

type LinkServiceInfo struct {
	Type        string `json:"type,omitempty"`
	QuotaPolicy string `json:"quota_policy,omitempty"`
}

type ShareOptions struct {
	Home                 string
	Thread               string
	Out                  string
	Name                 string
	Format               string
	Service              string
	Endpoint             string
	Token                string
	UnsafeIncludeSecrets bool
	S3                   S3Options
}

type S3Options struct {
	Endpoint        string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	Region          string
	PublicBaseURL   string
}

type ShareResult struct {
	Status      string     `json:"status"`
	Service     string     `json:"service"`
	ShareURL    string     `json:"share_url,omitempty"`
	ManifestURL string     `json:"manifest_url,omitempty"`
	ExpiresAt   string     `json:"expires_at,omitempty"`
	Fallback    string     `json:"fallback,omitempty"`
	Path        string     `json:"path,omitempty"`
	ThreadID    string     `json:"thread_id"`
	Title       string     `json:"title"`
	Safety      SafetyScan `json:"safety"`
	Bytes       int64      `json:"bytes"`
	Warnings    []string   `json:"warnings,omitempty"`
}

type WorkerCapabilities struct {
	Schema        string `json:"schema"`
	Service       string `json:"service"`
	MaxBlobBytes  int64  `json:"max_blob_bytes"`
	MaxTTLSeconds int64  `json:"max_ttl_seconds"`
	QuotaPolicy   string `json:"quota_policy"`
	AuthRequired  bool   `json:"auth_required"`
}

type workerShareResponse struct {
	ShareURL    string `json:"share_url"`
	ManifestURL string `json:"manifest_url"`
	ExpiresAt   string `json:"expires_at"`
}

type encryptedCapsule struct {
	Ciphertext []byte
	Key        []byte
	Nonce      []byte
	SHA256     string
}

func Share(opts ShareOptions) (*ShareResult, error) {
	format := strings.TrimSpace(opts.Format)
	if format == "" {
		format = LinkShareFormat
	}
	service := strings.TrimSpace(opts.Service)
	if service == "" {
		service = DefaultShareMode
	}
	if format == ZipShareFormat {
		result, err := Export(ExportOptions{
			Home:                 opts.Home,
			Thread:               opts.Thread,
			Out:                  opts.Out,
			Name:                 opts.Name,
			UnsafeIncludeSecrets: opts.UnsafeIncludeSecrets,
		})
		if err != nil {
			return nil, err
		}
		return &ShareResult{
			Status:   "zip",
			Service:  "zip",
			Path:     result.Path,
			ThreadID: result.ThreadID,
			Title:    result.Title,
			Safety:   result.Safety,
			Bytes:    result.Bytes,
		}, nil
	}
	if format != LinkShareFormat {
		return nil, fmt.Errorf("unsupported share format %q", format)
	}

	tempDir, err := os.MkdirTemp("", "capsule-share-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tempDir)
	tempZip := filepath.Join(tempDir, "session.capsule.zip")
	exported, err := Export(ExportOptions{
		Home:                 opts.Home,
		Thread:               opts.Thread,
		Out:                  tempZip,
		Name:                 opts.Name,
		UnsafeIncludeSecrets: opts.UnsafeIncludeSecrets,
	})
	if err != nil {
		return nil, err
	}
	plain, err := os.ReadFile(tempZip)
	if err != nil {
		return nil, err
	}
	enc, err := encryptCapsule(plain)
	if err != nil {
		return nil, err
	}
	shareID := uuid.NewString()
	manifest := buildLinkManifest(exported, enc, service)
	preview, err := buildEncryptedPreview(tempZip, enc.Key)
	if err != nil {
		return nil, err
	}
	manifest.Preview = preview

	var shareURL, manifestURL, expiresAt string
	var warnings []string
	switch service {
	case "official", "worker":
		shareURL, manifestURL, expiresAt, warnings, err = uploadViaWorker(context.Background(), opts, shareID, manifest, enc.Ciphertext)
	case "s3":
		shareURL, manifestURL, expiresAt, warnings, err = uploadViaS3(context.Background(), opts, shareID, manifest, enc.Ciphertext)
	default:
		err = fmt.Errorf("unsupported share service %q", service)
	}
	if err != nil {
		fallbackPath, copyErr := copyFallbackZip(tempZip, opts, exported)
		if copyErr != nil {
			return nil, fmt.Errorf("%w; additionally failed to write fallback zip: %v", err, copyErr)
		}
		return &ShareResult{
			Status:   "fallback_zip",
			Service:  service,
			Fallback: err.Error(),
			Path:     fallbackPath,
			ThreadID: exported.ThreadID,
			Title:    exported.Title,
			Safety:   exported.Safety,
			Bytes:    exported.Bytes,
			Warnings: warnings,
		}, nil
	}
	if shareURL != "" {
		shareURL = appendKeyFragment(shareURL, enc.Key)
	}
	return &ShareResult{
		Status:      "ok",
		Service:     service,
		ShareURL:    shareURL,
		ManifestURL: manifestURL,
		ExpiresAt:   expiresAt,
		ThreadID:    exported.ThreadID,
		Title:       exported.Title,
		Safety:      exported.Safety,
		Bytes:       exported.Bytes,
		Warnings:    warnings,
	}, nil
}

func RestoreAny(source string, opts codex.RestoreOptions) (*codex.RestoreResult, error) {
	if isHTTPURL(source) {
		return RestoreFromURL(source, opts)
	}
	return Restore(source, opts)
}

func RestoreFromURL(rawURL string, opts codex.RestoreOptions) (*codex.RestoreResult, error) {
	manifestURL, key, err := parseLinkKey(rawURL)
	if err != nil {
		return nil, err
	}
	manifest, err := fetchLinkManifest(context.Background(), manifestURL)
	if err != nil {
		return nil, err
	}
	if err := validateLinkManifest(manifest); err != nil {
		return nil, err
	}
	blobURL, err := resolveManifestURL(manifestURL, manifest.Bundle.URL)
	if err != nil {
		return nil, err
	}
	ciphertext, err := fetchBytes(context.Background(), blobURL)
	if err != nil {
		return nil, err
	}
	if int64(len(ciphertext)) != manifest.Bundle.Bytes {
		return nil, fmt.Errorf("ciphertext size mismatch: got %d, want %d", len(ciphertext), manifest.Bundle.Bytes)
	}
	digest := sha256.Sum256(ciphertext)
	if hex.EncodeToString(digest[:]) != manifest.Bundle.SHA256 {
		return nil, errors.New("ciphertext sha256 mismatch")
	}
	nonce, err := base64.RawURLEncoding.DecodeString(manifest.Crypto.Nonce)
	if err != nil {
		return nil, fmt.Errorf("decode nonce: %w", err)
	}
	plain, err := decryptCapsule(ciphertext, key, nonce)
	if err != nil {
		return nil, err
	}
	temp, err := os.CreateTemp("", "capsule-import-*.capsule.zip")
	if err != nil {
		return nil, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if _, err := temp.Write(plain); err != nil {
		_ = temp.Close()
		return nil, err
	}
	if err := temp.Close(); err != nil {
		return nil, err
	}
	return Restore(tempPath, opts)
}

func buildLinkManifest(exported *ExportResult, enc encryptedCapsule, service string) LinkManifest {
	createdAt := time.Now().UTC()
	return LinkManifest{
		Schema:    LinkSchema,
		CreatedAt: createdAt.Format(time.RFC3339Nano),
		Thread: LinkThread{
			ID:    exported.ThreadID,
			Title: exported.Title,
		},
		Bundle: LinkBundle{
			SHA256: enc.SHA256,
			Bytes:  int64(len(enc.Ciphertext)),
		},
		Crypto: LinkCrypto{
			Alg:    CryptoAES256GCM,
			Nonce:  base64.RawURLEncoding.EncodeToString(enc.Nonce),
			KeyRef: "url-fragment:k",
		},
		Import: LinkImport{
			Tool:           "capsule",
			Command:        "capsule import \"<this-url>\" --target codex --target-cwd . --execute",
			InstallCommand: InstallCmd,
			DryRunCommand:  "capsule import \"<this-url>\" --target codex --target-cwd .",
			ExecuteCommand: "capsule import \"<this-url>\" --target codex --target-cwd . --execute",
			DocsURL:        DefaultRepo,
		},
		Service: LinkServiceInfo{Type: service},
	}
}

func encryptCapsule(plain []byte) (encryptedCapsule, error) {
	key := make([]byte, 32)
	nonce := make([]byte, 12)
	if _, err := rand.Read(key); err != nil {
		return encryptedCapsule{}, err
	}
	if _, err := rand.Read(nonce); err != nil {
		return encryptedCapsule{}, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return encryptedCapsule{}, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return encryptedCapsule{}, err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, nil)
	digest := sha256.Sum256(ciphertext)
	return encryptedCapsule{
		Ciphertext: ciphertext,
		Key:        key,
		Nonce:      nonce,
		SHA256:     hex.EncodeToString(digest[:]),
	}, nil
}

func decryptCapsule(ciphertext, key, nonce []byte) ([]byte, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("invalid key length %d for %s", len(key), CryptoAES256GCM)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return gcm.Open(nil, nonce, ciphertext, nil)
}

func uploadViaWorker(ctx context.Context, opts ShareOptions, shareID string, manifest LinkManifest, blob []byte) (string, string, string, []string, error) {
	endpoint := strings.TrimRight(opts.Endpoint, "/")
	service := opts.Service
	if service == "official" && endpoint == "" {
		endpoint = strings.TrimRight(os.Getenv("CAPSULE_OFFICIAL_ENDPOINT"), "/")
	}
	if service == "worker" && endpoint == "" {
		endpoint = strings.TrimRight(os.Getenv("CAPSULE_WORKER_ENDPOINT"), "/")
	}
	if endpoint == "" {
		return "", "", "", nil, fmt.Errorf("%s worker endpoint is not configured", service)
	}
	token := opts.Token
	if token == "" {
		token = os.Getenv("CAPSULE_WORKER_TOKEN")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var warnings []string
	caps, err := workerCapabilities(ctx, client, endpoint, token)
	if err != nil {
		warnings = append(warnings, "worker capabilities unavailable; quota_policy=unknown")
	} else if caps.QuotaPolicy == "" || caps.QuotaPolicy == "unknown" || caps.QuotaPolicy == "unlimited" {
		warnings = append(warnings, "worker quota_policy="+defaultString(caps.QuotaPolicy, "unknown"))
	}
	payload, err := jsonBytes(manifest)
	if err != nil {
		return "", "", "", warnings, err
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("share_id", shareID); err != nil {
		return "", "", "", warnings, err
	}
	if err := writer.WriteField("manifest", string(payload)); err != nil {
		return "", "", "", warnings, err
	}
	part, err := writer.CreateFormFile("blob", "blob.enc")
	if err != nil {
		return "", "", "", warnings, err
	}
	if _, err := part.Write(blob); err != nil {
		return "", "", "", warnings, err
	}
	if err := writer.Close(); err != nil {
		return "", "", "", warnings, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/v1/shares", &body)
	if err != nil {
		return "", "", "", warnings, err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("Accept", "application/json")
	setBearer(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return "", "", "", warnings, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", "", warnings, fmt.Errorf("worker upload failed: %s %s", resp.Status, strings.TrimSpace(string(message)))
	}
	var out workerShareResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", "", warnings, err
	}
	if out.ShareURL == "" {
		return "", "", "", warnings, errors.New("worker response missing share_url")
	}
	return out.ShareURL, out.ManifestURL, out.ExpiresAt, warnings, nil
}

func workerCapabilities(ctx context.Context, client *http.Client, endpoint, token string) (*WorkerCapabilities, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v1/capabilities", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	setBearer(req, token)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("capabilities returned %s", resp.Status)
	}
	var caps WorkerCapabilities
	if err := json.NewDecoder(resp.Body).Decode(&caps); err != nil {
		return nil, err
	}
	return &caps, nil
}

func uploadViaS3(ctx context.Context, opts ShareOptions, shareID string, manifest LinkManifest, blob []byte) (string, string, string, []string, error) {
	cfg := resolveS3Options(opts.S3)
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKeyID == "" || cfg.SecretAccessKey == "" || cfg.PublicBaseURL == "" {
		return "", "", "", nil, errors.New("s3 service requires endpoint, bucket, access key, secret key, and public base url")
	}
	if cfg.Region == "" {
		cfg.Region = "auto"
	}
	prefix := strings.Trim(strings.TrimSpace(cfg.Prefix), "/")
	basePath := path.Join(prefix, shareID)
	blobKey := path.Join(basePath, "blob.enc")
	manifestKey := path.Join(basePath, "manifest.json")
	manifest.Bundle.URL = joinPublicURL(cfg.PublicBaseURL, blobKey)
	manifest.Service.Type = "s3"
	manifest.Service.QuotaPolicy = "user-managed"
	manifestURL := joinPublicURL(cfg.PublicBaseURL, manifestKey)
	payload, err := jsonBytes(manifest)
	if err != nil {
		return "", "", "", nil, err
	}
	client := &http.Client{Timeout: 30 * time.Second}
	if err := s3PutObject(ctx, client, cfg, blobKey, "application/octet-stream", blob); err != nil {
		return "", "", "", nil, fmt.Errorf("upload s3 blob: %w", err)
	}
	if err := s3PutObject(ctx, client, cfg, manifestKey, "application/json", payload); err != nil {
		return "", "", "", nil, fmt.Errorf("upload s3 manifest: %w", err)
	}
	warnings := []string{"s3 quota and lifecycle are managed by the BYO storage account"}
	return manifestURL, manifestURL, manifest.ExpiresAt, warnings, nil
}

func resolveS3Options(in S3Options) S3Options {
	out := in
	if out.Endpoint == "" {
		out.Endpoint = os.Getenv("CAPSULE_S3_ENDPOINT")
	}
	if out.Bucket == "" {
		out.Bucket = os.Getenv("CAPSULE_S3_BUCKET")
	}
	if out.Prefix == "" {
		out.Prefix = os.Getenv("CAPSULE_S3_PREFIX")
	}
	if out.AccessKeyID == "" {
		out.AccessKeyID = os.Getenv("CAPSULE_S3_ACCESS_KEY_ID")
	}
	if out.SecretAccessKey == "" {
		out.SecretAccessKey = os.Getenv("CAPSULE_S3_SECRET_ACCESS_KEY")
	}
	if out.Region == "" {
		out.Region = os.Getenv("CAPSULE_S3_REGION")
	}
	if out.PublicBaseURL == "" {
		out.PublicBaseURL = os.Getenv("CAPSULE_S3_PUBLIC_BASE_URL")
	}
	out.Endpoint = strings.TrimRight(out.Endpoint, "/")
	out.PublicBaseURL = strings.TrimRight(out.PublicBaseURL, "/")
	return out
}

func s3PutObject(ctx context.Context, client *http.Client, cfg S3Options, key, contentType string, body []byte) error {
	objectURL := strings.TrimRight(cfg.Endpoint, "/") + "/" + url.PathEscape(cfg.Bucket) + "/" + escapeS3Key(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, objectURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Amz-Content-Sha256", hashHex(body))
	signS3Request(req, cfg, body)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("%s %s", resp.Status, strings.TrimSpace(string(message)))
	}
	return nil
}

func signS3Request(req *http.Request, cfg S3Options, body []byte) {
	now := time.Now().UTC()
	date := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	payloadHash := hashHex(body)
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	host := req.URL.Host
	canonicalHeaders := "host:" + host + "\n" +
		"x-amz-content-sha256:" + payloadHash + "\n" +
		"x-amz-date:" + amzDate + "\n"
	signedHeaders := "host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := strings.Join([]string{
		req.Method,
		req.URL.EscapedPath(),
		req.URL.RawQuery,
		canonicalHeaders,
		signedHeaders,
		payloadHash,
	}, "\n")
	scope := strings.Join([]string{date, cfg.Region, "s3", "aws4_request"}, "/")
	hashedCanonical := sha256.Sum256([]byte(canonicalRequest))
	stringToSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hex.EncodeToString(hashedCanonical[:])
	signingKey := awsSigningKey(cfg.SecretAccessKey, date, cfg.Region, "s3")
	signature := hmacSHA256Hex(signingKey, stringToSign)
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+cfg.AccessKeyID+"/"+scope+", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func awsSigningKey(secret, date, region, service string) []byte {
	kDate := hmacSHA256Bytes([]byte("AWS4"+secret), date)
	kRegion := hmacSHA256Bytes(kDate, region)
	kService := hmacSHA256Bytes(kRegion, service)
	return hmacSHA256Bytes(kService, "aws4_request")
}

func hmacSHA256Bytes(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}

func hmacSHA256Hex(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256Bytes(key, data))
}

func hashHex(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func fetchLinkManifest(ctx context.Context, manifestURL string) (LinkManifest, error) {
	var manifest LinkManifest
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return manifest, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return manifest, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return manifest, fmt.Errorf("fetch manifest failed: %s %s", resp.Status, strings.TrimSpace(string(message)))
	}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func fetchBytes(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("fetch blob failed: %s %s", resp.Status, strings.TrimSpace(string(message)))
	}
	return io.ReadAll(resp.Body)
}

func validateLinkManifest(manifest LinkManifest) error {
	if manifest.Schema != LinkSchema {
		return fmt.Errorf("unsupported link schema %q", manifest.Schema)
	}
	if manifest.Bundle.URL == "" {
		return errors.New("link manifest missing bundle.url")
	}
	if manifest.Bundle.SHA256 == "" {
		return errors.New("link manifest missing bundle.sha256")
	}
	if manifest.Crypto.Alg != CryptoAES256GCM {
		return fmt.Errorf("unsupported link crypto algorithm %q", manifest.Crypto.Alg)
	}
	if manifest.Crypto.Nonce == "" {
		return errors.New("link manifest missing crypto.nonce")
	}
	if manifest.Crypto.KeyRef != "url-fragment:k" {
		return fmt.Errorf("unsupported key ref %q", manifest.Crypto.KeyRef)
	}
	return nil
}

func parseLinkKey(rawURL string) (string, []byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, err
	}
	values, err := url.ParseQuery(u.Fragment)
	if err != nil {
		return "", nil, fmt.Errorf("parse link fragment: %w", err)
	}
	keyText := values.Get("k")
	if keyText == "" {
		return "", nil, errors.New("link is missing #k= decryption key")
	}
	key, err := base64.RawURLEncoding.DecodeString(keyText)
	if err != nil {
		return "", nil, fmt.Errorf("decode link key: %w", err)
	}
	u.Fragment = ""
	return u.String(), key, nil
}

func resolveManifestURL(manifestURL, value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return "", err
	}
	if parsed.IsAbs() {
		return parsed.String(), nil
	}
	base, err := url.Parse(manifestURL)
	if err != nil {
		return "", err
	}
	return base.ResolveReference(parsed).String(), nil
}

func copyFallbackZip(tempZip string, opts ShareOptions, exported *ExportResult) (string, error) {
	out := opts.Out
	if out == "" {
		out = DefaultOutputName(opts.Name, exported.Title, "", exported.ThreadID)
	}
	content, err := os.ReadFile(tempZip)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(out)), 0o755); err != nil && filepath.Dir(filepath.Clean(out)) != "." {
		return "", err
	}
	if err := os.WriteFile(out, content, 0o644); err != nil {
		return "", err
	}
	return out, nil
}

func appendKeyFragment(rawURL string, key []byte) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL + "#k=" + base64.RawURLEncoding.EncodeToString(key)
	}
	values, _ := url.ParseQuery(u.Fragment)
	values.Set("k", base64.RawURLEncoding.EncodeToString(key))
	u.Fragment = values.Encode()
	return u.String()
}

func joinPublicURL(base, key string) string {
	return strings.TrimRight(base, "/") + "/" + escapeS3Key(key)
}

func escapeS3Key(key string) string {
	parts := strings.Split(strings.TrimLeft(key, "/"), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func isHTTPURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

func setBearer(req *http.Request, token string) {
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
