package vc

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
)

// Cloudflare R2 is S3-compatible, so we use the aws-sdk-go v1 client
// (already in our transitive deps) pointed at R2's endpoint. R2
// charges $0 egress, which is the whole reason for this fan-out
// system — a chartplotter fleet at 10K instances would otherwise
// crush ECMWF Open Data's free tier inside a week.
//
// Auth uses an R2 API token (Access Key ID + Secret Access Key) tied
// to a specific bucket. Region must be "auto" for R2.

// R2Config wires the publisher to one R2 bucket.
//
// Auth: Cloudflare R2 API tokens have an `id` (used as the S3
// Access Key ID) and a `value` (a long secret string; SHA-256 of
// that gives the SigV4 Secret Access Key). This struct supports
// three increasingly convenient forms:
//
//   - Pass `AccessKeyID` + `SecretAccessKey` directly (legacy /
//     for setups that already store the derived secret).
//   - Pass `AccessKeyID` + `APIToken` (the raw token value); the
//     constructor SHA-256s the value to get the secret.
//   - Pass `APIToken` ALONE; the constructor calls Cloudflare's
//     `/user/tokens/verify` to discover the token's `id` and SHA-
//     256s the value. Most convenient — one secret to manage —
//     but adds a one-time HTTPS round-trip on uploader construct.
//
// All three are equivalent — pick whichever you prefer based on
// what your secrets store gives you.
type R2Config struct {
	AccountID       string // your Cloudflare account ID
	AccessKeyID     string // R2 API token's id field (visible in the dashboard)
	SecretAccessKey string // optional: SHA-256(token value); derived from APIToken when empty
	APIToken        string // optional: raw R2 API token value; hashed into SecretAccessKey
	Bucket          string // bucket name
}

// Validate fails on a missing required field. Called by every
// constructor so a misconfigured publisher errors at startup instead
// of at the first upload.
func (c R2Config) Validate() error {
	switch {
	case c.AccountID == "":
		return fmt.Errorf("r2: account_id required")
	case c.APIToken == "" && (c.AccessKeyID == "" || c.SecretAccessKey == ""):
		return fmt.Errorf("r2: provide either api_token (alone — id derived via Cloudflare /verify) " +
			"or access_key_id + (api_token | secret_access_key)")
	case c.Bucket == "":
		return fmt.Errorf("r2: bucket required")
	}
	return nil
}

// resolvedSecret returns the SigV4 secret key, deriving it from the
// raw API token value when the pre-hashed form isn't supplied. The
// SHA-256 conversion matches Cloudflare's documented derivation:
// "Secret Access Key = SHA-256 hash of the API token value".
func (c R2Config) resolvedSecret() string {
	if c.SecretAccessKey != "" {
		return c.SecretAccessKey
	}
	sum := sha256.Sum256([]byte(c.APIToken))
	return hex.EncodeToString(sum[:])
}

// R2Uploader pushes one PublishedCycle to a Cloudflare R2 bucket.
// Safe for concurrent UploadCycle calls but a single cycle's uploads
// are parallel-bounded internally.
type R2Uploader struct {
	cfg R2Config
	s3  *s3.S3
}

// NewR2Uploader builds the uploader against an explicit config. Use
// NewR2UploaderFromEnv when invoking from a CLI / Viam resource where
// secrets live in env vars or attributes.
func NewR2Uploader(cfg R2Config) (*R2Uploader, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	// When the caller passed an API token but no Access Key ID, ask
	// Cloudflare for the token's id and use it. One HTTPS round-trip
	// at construct time; the result is cached for the uploader's
	// lifetime.
	if cfg.AccessKeyID == "" {
		id, err := cloudflareVerifyTokenID(context.Background(), cfg.APIToken)
		if err != nil {
			return nil, fmt.Errorf("r2: derive access_key_id from api_token: %w", err)
		}
		cfg.AccessKeyID = id
		log.Printf("r2: derived access_key_id=%s from api_token via cloudflare /verify", id)
	}
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String("auto"),
		Endpoint:    aws.String(endpoint),
		Credentials: credentials.NewStaticCredentials(cfg.AccessKeyID, cfg.resolvedSecret(), ""),
		// R2 doesn't support virtual-host style addressing for buckets
		// that contain dots; force path-style so any bucket name works.
		S3ForcePathStyle: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("r2 session: %w", err)
	}
	return &R2Uploader{cfg: cfg, s3: s3.New(sess)}, nil
}

// cloudflareVerifyTokenID hits Cloudflare's /user/tokens/verify
// endpoint with the supplied Bearer token. The response includes the
// token's `id`, which doubles as the S3 Access Key ID for R2's
// SigV4 endpoint. Lets a publisher operator configure with just one
// secret (the API token) instead of two (id + value).
//
// Errors are passed back verbatim from Cloudflare so a misconfigured
// or expired token surfaces with the actual Cloudflare error message
// rather than a generic "auth failed" later in the upload path.
func cloudflareVerifyTokenID(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET",
		"https://api.cloudflare.com/client/v4/user/tokens/verify", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var v struct {
		Result struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"result"`
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("cloudflare verify: parse: %w (body: %s)", err, body)
	}
	if !v.Success || v.Result.ID == "" {
		// Code 1000 "Invalid API Token" from /verify almost always
		// means the user has an R2-specific S3 credential (created
		// via R2 → Manage R2 API Tokens) rather than a generic
		// Cloudflare API token (created via My Profile → API Tokens).
		// The R2 dashboard confusingly labels both as "API Token" but
		// only the latter works with /verify. Give an actionable hint
		// instead of just echoing the Cloudflare error.
		for _, e := range v.Errors {
			if e.Code == 1000 {
				return "", fmt.Errorf(
					"cloudflare /verify rejected this token (code 1000 'Invalid API Token'). " +
						"This usually means you have an R2-specific S3 credential (from R2 → " +
						"Manage R2 API Tokens), not a generic Cloudflare API token. R2-specific " +
						"tokens can't be verified via /verify — the dashboard shows BOTH an " +
						"Access Key ID and a Secret Access Key on the same result screen. Set " +
						"both: R2_ACCESS_KEY_ID=<the short string> and either " +
						"R2_SECRET_ACCESS_KEY=<the pre-hashed long string> or R2_API_TOKEN=<the " +
						"raw token value if you saved it before the secret reveal closed>")
			}
		}
		return "", fmt.Errorf("cloudflare verify: %v (body: %s)", v.Errors, body)
	}
	if v.Result.Status != "active" {
		return "", fmt.Errorf("cloudflare verify: token status=%s (not active)", v.Result.Status)
	}
	return v.Result.ID, nil
}

// NewR2UploaderFromEnv reads R2_ACCOUNT_ID, R2_ACCESS_KEY_ID, and
// either R2_API_TOKEN (raw token value, hashed into the SigV4 secret)
// or R2_SECRET_ACCESS_KEY (pre-hashed) from the process environment.
// R2_BUCKET is optional — falls back to DefaultECMWFR2Bucket.
// Convenient for cmd/wind-publisher and dev workflows; in production
// the Viam resource constructor reads from component attributes
// instead.
func NewR2UploaderFromEnv() (*R2Uploader, error) {
	bucket := os.Getenv("R2_BUCKET")
	if bucket == "" {
		bucket = DefaultECMWFR2Bucket
	}
	return NewR2Uploader(R2Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		APIToken:        os.Getenv("R2_API_TOKEN"),
		Bucket:          bucket,
	})
}

// uploadConcurrency caps in-flight PUTs per UploadCycle. 8 is enough
// to saturate a typical home-broadband uplink without flooding R2's
// per-bucket request quota. Tuned conservatively because a publisher
// crash mid-upload should be rare; if it happens the next cron run
// just re-uploads the cycle (object writes are idempotent and the
// latest pointer flips last).
const uploadConcurrency = 8

// UploadCycle uploads every (fh, tile) blob plus the per-cycle
// manifest, and only then atomically updates wind/<model>/latest.json
// to point at the new cycle. If any blob upload fails the latest
// pointer is left alone, so chartplotters keep reading the prior
// cycle until the next cron run succeeds.
func (u *R2Uploader) UploadCycle(ctx context.Context, cycle *PublishedCycle) error {
	cycleStr := cycle.CycleTime.UTC().Format("20060102T15")
	totalBytes := 0

	// Upload every tile blob first.
	type job struct {
		key  string
		blob TileBlob
	}
	jobs := make(chan job)
	errs := make(chan error, uploadConcurrency)
	var wg sync.WaitGroup
	wg.Add(uploadConcurrency)
	for w := 0; w < uploadConcurrency; w++ {
		go func() {
			defer wg.Done()
			for j := range jobs {
				if err := u.putBlob(ctx, j.key, j.blob.GzippedJSON,
					"application/json", "gzip",
					"public, max-age=31536000, immutable"); err != nil {
					select {
					case errs <- fmt.Errorf("put %s: %w", j.key, err):
					default:
					}
					return
				}
			}
		}()
	}
	for _, fh := range cycle.FHs {
		for _, tile := range cycle.Tiles {
			blob, ok := cycle.TileBlobFor(fh, tile.Key)
			if !ok {
				close(jobs)
				wg.Wait()
				return fmt.Errorf("missing blob fh=%d tile=%s", fh, tile.Key)
			}
			totalBytes += len(blob.GzippedJSON)
			key := fmt.Sprintf("wind/%s/%s/f%03d/%s.json.gz", cycle.Model, cycleStr, fh, tile.Key)
			select {
			case jobs <- job{key: key, blob: blob}:
			case <-ctx.Done():
				close(jobs)
				wg.Wait()
				return ctx.Err()
			}
		}
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			return err
		}
	}

	// Per-cycle manifest (immutable).
	manifestBody, err := json.MarshalIndent(cycle.Manifest(), "", "  ")
	if err != nil {
		return fmt.Errorf("manifest marshal: %w", err)
	}
	manifestKey := fmt.Sprintf("wind/%s/manifest/%s.json", cycle.Model, cycleStr)
	if err := u.putBlob(ctx, manifestKey, manifestBody,
		"application/json", "",
		"public, max-age=31536000, immutable"); err != nil {
		return fmt.Errorf("put manifest: %w", err)
	}

	// Latest pointer — short TTL, atomic from the client's perspective
	// (S3/R2 PUT replaces the object atomically; readers see either
	// the old version or the new one, never a partial).
	latestKey := fmt.Sprintf("wind/%s/latest.json", cycle.Model)
	if err := u.putBlob(ctx, latestKey, manifestBody,
		"application/json", "",
		"public, max-age=60"); err != nil {
		return fmt.Errorf("put latest: %w", err)
	}

	log.Printf("r2: published cycle %s/%s — %d tile blobs (%.1f MB gzipped) + manifest in %d files",
		cycle.Model, cycleStr,
		len(cycle.FHs)*len(cycle.Tiles), float64(totalBytes)/(1024*1024),
		len(cycle.FHs)*len(cycle.Tiles)+2)
	return nil
}

// putBlob does one PutObject with the standard headers. Separated so
// the tile-upload worker pool and the manifest/pointer writes share
// the same retry-and-error path.
func (u *R2Uploader) putBlob(ctx context.Context, key string, body []byte, contentType, contentEncoding, cacheControl string) error {
	in := &s3.PutObjectInput{
		Bucket:       aws.String(u.cfg.Bucket),
		Key:          aws.String(key),
		Body:         bytes.NewReader(body),
		ContentType:  aws.String(contentType),
		CacheControl: aws.String(cacheControl),
	}
	if contentEncoding != "" {
		in.ContentEncoding = aws.String(contentEncoding)
	}
	// Modest retry: R2 occasionally returns 5xx under load. Three
	// tries with linear backoff is enough — a cycle of 1700+ uploads
	// won't tolerate aggressive retries without blowing past the cron
	// window.
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, err := u.s3.PutObjectWithContext(ctx, in)
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return lastErr
}
