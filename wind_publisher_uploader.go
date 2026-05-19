package vc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
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
type R2Config struct {
	AccountID       string // your Cloudflare account ID
	AccessKeyID     string // R2 API token access key
	SecretAccessKey string // R2 API token secret
	Bucket          string // bucket name
}

// Validate fails on a missing required field. Called by every
// constructor so a misconfigured publisher errors at startup instead
// of at the first upload.
func (c R2Config) Validate() error {
	switch {
	case c.AccountID == "":
		return fmt.Errorf("r2: account_id required")
	case c.AccessKeyID == "":
		return fmt.Errorf("r2: access_key_id required")
	case c.SecretAccessKey == "":
		return fmt.Errorf("r2: secret_access_key required")
	case c.Bucket == "":
		return fmt.Errorf("r2: bucket required")
	}
	return nil
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
	endpoint := fmt.Sprintf("https://%s.r2.cloudflarestorage.com", cfg.AccountID)
	sess, err := session.NewSession(&aws.Config{
		Region:      aws.String("auto"),
		Endpoint:    aws.String(endpoint),
		Credentials: credentials.NewStaticCredentials(cfg.AccessKeyID, cfg.SecretAccessKey, ""),
		// R2 doesn't support virtual-host style addressing for buckets
		// that contain dots; force path-style so any bucket name works.
		S3ForcePathStyle: aws.Bool(true),
	})
	if err != nil {
		return nil, fmt.Errorf("r2 session: %w", err)
	}
	return &R2Uploader{cfg: cfg, s3: s3.New(sess)}, nil
}

// NewR2UploaderFromEnv reads R2_ACCOUNT_ID / R2_ACCESS_KEY_ID /
// R2_SECRET_ACCESS_KEY / R2_BUCKET from the process environment.
// Convenient for cmd/wind-publisher and dev workflows; in production
// the Viam resource constructor reads from component attributes
// instead.
func NewR2UploaderFromEnv() (*R2Uploader, error) {
	return NewR2Uploader(R2Config{
		AccountID:       os.Getenv("R2_ACCOUNT_ID"),
		AccessKeyID:     os.Getenv("R2_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("R2_SECRET_ACCESS_KEY"),
		Bucket:          os.Getenv("R2_BUCKET"),
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
