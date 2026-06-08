package vc

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.viam.com/rdk/components/generic"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/resource"
)

// Wind-publisher Viam resource model: configure on exactly one
// machine in your fleet (the "publisher"). Every other chartplotter
// reads from the CDN this writes to, so a 10K-chartplotter fleet
// hits ECMWF Open Data four times a day total instead of 40K times.
//
// The model is `erh:viam-chartplotter:wind-publisher`. Picking the
// publisher machine is purely a config decision: just add this
// component to the one machine you want to run it.

// WindPublisherModel is the Viam model triple for the publisher.
var WindPublisherModel = resource.ModelNamespace("erh").WithFamily("viam-chartplotter").WithModel("wind-publisher")

// DefaultECMWFR2Bucket is the canonical Cloudflare R2 bucket the
// project-wide publisher writes to. Hardcoded so every chartplotter
// in the fleet reads from the same place by default; an operator can
// still override via the `r2_bucket` config attribute if they're
// running a staging fleet or want to sandbox a publisher's output.
const DefaultECMWFR2Bucket = "viam-chartplotter-ecmwf"

// DefaultWindCDNBaseURL is the public r2.dev URL the
// viam-chartplotter-ecmwf bucket is exposed at. Every chartplotter
// in the fleet defaults to fetching ECMWF tiles from here so we
// only hit ECMWF Open Data from the one machine running the
// wind-publisher component, not from 10K chartplotters. Override
// via the `wind_cdn_base_url` chartplotter config attribute when
// running against a staging bucket or a custom-domain mirror.
const DefaultWindCDNBaseURL = "https://pub-6ae2d2a870f74799a963dbc892ea400b.r2.dev"

func init() {
	resource.RegisterComponent(
		generic.API,
		WindPublisherModel,
		resource.Registration[resource.Resource, *WindPublisherConfig]{
			Constructor: newWindPublisher,
		})
}

// WindPublisherConfig configures one publisher instance. The R2
// credentials live here rather than in env vars so a Viam config
// can fully describe one machine's role.
type WindPublisherConfig struct {
	// Models the publisher should produce. Currently only "ecmwf" is
	// implemented; "gfs", "icon-eu" etc. will plug into the same
	// BuildECMWFCycle-style entry point as we ship them.
	Models []string `json:"models"`

	// R2 credentials. The cleanest setup uses two values from
	// Cloudflare's "Create R2 API Token" dialog:
	//   r2_access_key_id  → the token's *id* (a short identifier
	//                       Cloudflare shows alongside the token)
	//   r2_api_token      → the raw token *value* (a long secret);
	//                       SHA-256'd at startup to produce the
	//                       SigV4 secret S3 needs.
	// r2_secret_access_key is the legacy alternative — supply it
	// directly only if you've already computed it yourself.
	// AccountID + AccessKeyID + (APIToken OR SecretAccessKey) are
	// required when UploadEnabled = true. Bucket defaults to
	// DefaultECMWFR2Bucket; override only for a staging fleet.
	R2AccountID       string `json:"r2_account_id"`
	R2AccessKeyID     string `json:"r2_access_key_id"`
	R2APIToken        string `json:"r2_api_token,omitempty"`
	R2SecretAccessKey string `json:"r2_secret_access_key,omitempty"`
	R2Bucket          string `json:"r2_bucket,omitempty"`

	// UploadEnabled toggles real R2 uploads. Default false so adding
	// the component to a new machine doesn't immediately start
	// writing to production; flip to true once credentials are
	// verified. Useful staging knob.
	UploadEnabled bool `json:"upload_enabled,omitempty"`

	// PublishOffsetMinutes shifts the post-cycle wake-up to give
	// ECMWF time to finish publishing. Defaults to 30 (cron fires at
	// :30 past the hour, ~7.5h after each cycle reference). Lower
	// only if you've verified ECMWF publishes earlier.
	PublishOffsetMinutes int `json:"publish_offset_minutes,omitempty"`
}

// Validate enforces required fields when UploadEnabled is on. The
// component's lifecycle calls Validate before Construct; failing here
// prevents an unconfigured publisher from binding (and starting a
// cron loop that would just no-op on every wake).
func (c *WindPublisherConfig) Validate(path string) ([]string, error) {
	if len(c.Models) == 0 {
		return nil, fmt.Errorf("%s: models required (e.g. [\"ecmwf\"])", path)
	}
	for _, m := range c.Models {
		if m != "ecmwf" {
			return nil, fmt.Errorf("%s: model %q not yet supported by publisher (only ecmwf)", path, m)
		}
	}
	if c.UploadEnabled {
		switch {
		case c.R2AccountID == "":
			return nil, fmt.Errorf("%s: r2_account_id required when upload_enabled", path)
		case c.R2APIToken == "" && (c.R2AccessKeyID == "" || c.R2SecretAccessKey == ""):
			return nil, fmt.Errorf("%s: provide r2_api_token alone (id derived via Cloudflare /verify) "+
				"or r2_access_key_id + (r2_api_token | r2_secret_access_key)", path)
		}
	}
	return nil, nil
}

// effectiveBucket returns the configured bucket or the project-wide
// default. Kept in one place so reconfigure() and any future
// inspection RPC report the same name.
func (c *WindPublisherConfig) effectiveBucket() string {
	if c.R2Bucket != "" {
		return c.R2Bucket
	}
	return DefaultECMWFR2Bucket
}

// windPublisher is the running resource. It owns one goroutine that
// sleeps until the next scheduled wake-up, then runs through each
// configured model. Close() cancels the goroutine; Reconfigure swaps
// the cancel context so a config change kills in-flight work.
type windPublisher struct {
	resource.AlwaysRebuild
	resource.Named

	logger logging.Logger

	mu       sync.Mutex
	cfg      *WindPublisherConfig
	uploader *R2Uploader
	cancel   context.CancelFunc

	// lastPublish tracks the most recent (model, cycle) we successfully
	// uploaded so a re-wake within the same publishing window skips
	// redundant work.
	lastPublish map[string]string // model → cycle string
}

func newWindPublisher(ctx context.Context, deps resource.Dependencies, conf resource.Config, logger logging.Logger) (resource.Resource, error) {
	cfg, err := resource.NativeConfig[*WindPublisherConfig](conf)
	if err != nil {
		return nil, err
	}
	// Wire the project-wide raw-grib cache (rule: every external
	// fetch goes through a disk cache) if no other code path has
	// already set it. On a machine that also runs the chartplotter
	// component, NewWeatherCache will have set this; on a
	// publisher-only machine this is the only place that does.
	if ECMWFRawCacheDir == "" {
		base, derr := os.UserCacheDir()
		if derr != nil {
			base = os.TempDir()
		}
		dir := filepath.Join(base, "viam-chartplotter-wind-publisher", "raw-ecmwf")
		if serr := SetECMWFRawCacheDir(dir); serr != nil {
			logger.Warnf("publisher: raw-grib cache disabled: %v", serr)
		} else {
			logger.Infof("publisher: raw-grib cache: %s", dir)
		}
	}
	p := &windPublisher{
		Named:       conf.ResourceName().AsNamed(),
		logger:      logger,
		lastPublish: map[string]string{},
	}
	if err := p.reconfigure(cfg); err != nil {
		return nil, err
	}
	return p, nil
}

// reconfigure handles both initial construction and any future
// Reconfigure call (AlwaysRebuild keeps it init-only for now, but the
// shape is here for when we relax that).
func (p *windPublisher) reconfigure(cfg *WindPublisherConfig) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.cancel()
	}
	p.cfg = cfg
	if cfg.UploadEnabled {
		up, err := NewR2Uploader(R2Config{
			AccountID:       cfg.R2AccountID,
			AccessKeyID:     cfg.R2AccessKeyID,
			APIToken:        cfg.R2APIToken,
			SecretAccessKey: cfg.R2SecretAccessKey,
			Bucket:          cfg.effectiveBucket(),
		})
		if err != nil {
			return fmt.Errorf("r2 setup: %w", err)
		}
		p.uploader = up
		p.logger.Infof("publisher: r2 bucket=%s", cfg.effectiveBucket())
	} else {
		p.uploader = nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	go p.runLoop(ctx)
	return nil
}

// Close stops the cron goroutine. In-flight uploads see ctx.Done()
// and unwind on their next R2 call.
func (p *windPublisher) Close(ctx context.Context) error {
	p.mu.Lock()
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.mu.Unlock()
	return nil
}

// runLoop is the publisher's cron core. Wakes up at each scheduled
// publish window, drives each model through BuildECMWFCycle +
// UploadCycle, then sleeps until the next window. Built on top of a
// 15-minute heartbeat instead of computing exact next-wake time so a
// missed cycle (ECMWF still publishing, network blip, etc.) gets
// re-tried roughly every 15 min within the window.
func (p *windPublisher) runLoop(ctx context.Context) {
	const heartbeat = 15 * time.Minute
	// First wake: immediate so a freshly-reconfigured publisher
	// attempts a publish without waiting for the heartbeat.
	wake := time.NewTimer(0)
	defer wake.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-wake.C:
		}
		p.runOnce(ctx)
		wake.Reset(heartbeat)
	}
}

// runOnce probes whether we're in a publish window for any configured
// model and, if so, runs the publish. Idempotent: a re-entry within
// the same cycle is a no-op thanks to the lastPublish memo.
func (p *windPublisher) runOnce(ctx context.Context) {
	p.mu.Lock()
	cfg := p.cfg
	uploader := p.uploader
	p.mu.Unlock()
	if cfg == nil {
		return
	}
	for _, modelName := range cfg.Models {
		if err := ctx.Err(); err != nil {
			return
		}
		p.maybePublishOne(ctx, cfg, uploader, modelName)
	}
}

// maybePublishOne checks whether the *current* cycle for `modelName`
// is older than what we've already published; if so, runs a full
// build + upload. Errors are logged and swallowed — the next
// heartbeat retries.
func (p *windPublisher) maybePublishOne(ctx context.Context, cfg *WindPublisherConfig, uploader *R2Uploader, modelName string) {
	m := FindWeatherModelForPublish(modelName)
	if m == nil {
		p.logger.Warnf("publisher: unknown model %q", modelName)
		return
	}

	// What cycle should be available right now? walkLatestCycle uses
	// the same PublishLagH-derived logic; querying it here avoids
	// hard-coding cycle timestamps in the publisher.
	now := time.Now().UTC().Add(-time.Duration(m.PublishLagH) * time.Hour)
	currentCycle := mostRecentCycle(now, m.CycleHours)
	currentStr := currentCycle.Format("20060102T15")

	p.mu.Lock()
	last := p.lastPublish[modelName]
	p.mu.Unlock()
	if last == currentStr {
		// Already published this cycle. The heartbeat will check
		// again next interval; nothing else to do.
		return
	}

	p.logger.Infof("publisher: starting %s cycle=%s build", modelName, currentStr)
	t0 := time.Now()
	client := &http.Client{Timeout: 120 * time.Second}
	cycle, err := BuildECMWFCycle(ctx, client, m)
	if err != nil {
		p.logger.Warnf("publisher: build %s cycle=%s: %v", modelName, currentStr, err)
		return
	}
	resolvedStr := cycle.CycleTime.UTC().Format("20060102T15")
	if resolvedStr != currentStr {
		// walkLatestCycle fell back to an older cycle (the freshest
		// one is still publishing). Don't memoise as "current" — try
		// again next heartbeat.
		p.logger.Infof("publisher: %s resolved to older cycle=%s (wanted %s); will retry",
			modelName, resolvedStr, currentStr)
	}

	if uploader != nil {
		if err := uploader.UploadCycle(ctx, cycle); err != nil {
			p.logger.Warnf("publisher: upload %s cycle=%s: %v", modelName, resolvedStr, err)
			return
		}
	} else {
		p.logger.Infof("publisher: upload disabled — built cycle %s/%s with %d blobs in %s but skipping push",
			modelName, resolvedStr, len(cycle.FHs)*len(cycle.Tiles), time.Since(t0).Round(time.Second))
	}

	p.mu.Lock()
	p.lastPublish[modelName] = resolvedStr
	p.mu.Unlock()
	p.logger.Infof("publisher: %s cycle=%s done in %s", modelName, resolvedStr, time.Since(t0).Round(time.Second))
}

// DoCommand exposes a small inspection / manual-trigger API for
// debugging from `viam-cli component do`. Commands:
//
//	{"command": "status"}              → last-published cycles, config
//	{"command": "publish_now"}         → fire a publish loop immediately
//	{"command": "publish_now", "model": "ecmwf"} → same, one model only
func (p *windPublisher) DoCommand(ctx context.Context, cmd map[string]interface{}) (map[string]interface{}, error) {
	op, _ := cmd["command"].(string)
	switch op {
	case "", "status":
		p.mu.Lock()
		last := map[string]string{}
		for k, v := range p.lastPublish {
			last[k] = v
		}
		var models []string
		uploadEnabled := false
		if p.cfg != nil {
			models = append(models, p.cfg.Models...)
			uploadEnabled = p.cfg.UploadEnabled
		}
		p.mu.Unlock()
		return map[string]interface{}{
			"models":        models,
			"uploadEnabled": uploadEnabled,
			"lastPublished": last,
		}, nil
	case "publish_now":
		modelFilter, _ := cmd["model"].(string)
		p.mu.Lock()
		cfg := p.cfg
		uploader := p.uploader
		p.mu.Unlock()
		if cfg == nil {
			return nil, fmt.Errorf("publisher not configured")
		}
		modelsToRun := cfg.Models
		if modelFilter != "" {
			modelsToRun = []string{modelFilter}
		}
		go func() {
			for _, mn := range modelsToRun {
				p.maybePublishOne(ctx, cfg, uploader, strings.TrimSpace(mn))
			}
		}()
		return map[string]interface{}{"triggered": modelsToRun}, nil
	default:
		return nil, fmt.Errorf("unknown command %q", op)
	}
}
