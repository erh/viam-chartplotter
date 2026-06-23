package render

import (
	"context"
	"time"

	"go.viam.com/rdk/logging"
)

// Prewarm pacing. The sweep is deliberately gentle: it only renders while the
// server has been completely quiet (no /noaa-enc/* requests) for
// prewarmIdleWindow, and never starts more than one tile per
// prewarmMinInterval — a real user panning the map always wins.
const (
	prewarmIdleWindow  = 30 * time.Second
	prewarmMinInterval = time.Second
	// prewarmIdlePollMax caps how long we sleep between idleness re-checks so
	// a cancelled context is noticed promptly.
	prewarmIdlePollMax = 5 * time.Second
)

// noteRequest stamps "a user asked for something just now". Every handler
// registered by Register goes through this, so the prewarmer's idle check
// sees any tile/debug/compare traffic. The prewarmer itself calls the
// renderer directly and does NOT stamp.
func (h *ENCHandlers) noteRequest() {
	h.lastRequest.Store(time.Now().UnixNano())
}

func (h *ENCHandlers) sinceLastRequest() time.Duration {
	return time.Since(time.Unix(0, h.lastRequest.Load()))
}

// StartPrewarm launches a background sweep that renders every tile of each
// zoom in `zooms` (in order) covering the lon/lat bbox [minLon, minLat,
// maxLon, maxLat] into the tile cache, then exits. Tiles already cached for
// the current render-rules version are skipped, so after one completed sweep
// (the cache is on disk) a restart costs only cache probes.
//
// Tiles render with BrowserMergedOptions + the module's default safe depth —
// the exact options/cache shard the frontend's overview band requests — so a
// user panning the coast hits warm tiles.
//
// Call StopPrewarm (idempotent) on shutdown.
func (h *ENCHandlers) StartPrewarm(bbox [4]float64, zooms []int) {
	if h.tileCache == nil || h.renderer == nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.prewarmCancel = cancel
	go h.prewarmLoop(ctx, bbox, zooms)
}

// StopPrewarm cancels a running prewarm sweep. Safe to call when none is
// running or after the sweep finished.
func (h *ENCHandlers) StopPrewarm() {
	if h.prewarmCancel != nil {
		h.prewarmCancel()
	}
}

func (h *ENCHandlers) prewarmLoop(ctx context.Context, bbox [4]float64, zooms []int) {
	logger := h.renderer.Logger()
	bucket := safeDepthBucket(h.defaultSafeDepth)
	safeDepthM := float64(bucket) / feetPerMetre

	rendered, skipped, failed := 0, 0, 0
	start := time.Now()
	for _, z := range zooms {
		// Top-left and bottom-right tiles of the bbox (y grows southward).
		x0, y0 := lonLatToTile(bbox[0], bbox[3], z)
		x1, y1 := lonLatToTile(bbox[2], bbox[1], z)
		if logger != nil {
			logger.Infof("tile prewarm: z%d sweep %dx%d tiles (x%d..%d y%d..%d)",
				z, x1-x0+1, y1-y0+1, x0, x1, y0, y1)
		}
		opts := BrowserMergedOptions(z, safeDepthM)
		cacheKey := tileCacheKey(opts.Style, opts.SkipNavaids, opts.TransparentLand, opts.SkipClasses)
		for y := y0; y <= y1; y++ {
			for x := x0; x <= x1; x++ {
				if _, ok := h.tileCache.Get(cacheKey, bucket, z, x, y); ok {
					skipped++
					continue
				}
				if !h.prewarmWaitIdle(ctx, logger) {
					return // cancelled
				}
				t0 := time.Now()
				pngBytes, _, _, err := h.renderer.RenderMergedTile(z, x, y, opts)
				switch {
				case err != nil:
					failed++
					if logger != nil {
						logger.Warnf("tile prewarm: render z=%d x=%d y=%d: %v", z, x, y, err)
					}
				case len(pngBytes) >= prewarmMinCacheableTileBytes:
					// Same empty-tile rule as handleTile: a near-empty PNG means
					// "nothing to draw here" and isn't worth pinning in the cache.
					if err := h.tileCache.Put(cacheKey, bucket, z, x, y, pngBytes); err == nil {
						rendered++
						if logger != nil {
							logger.Infof("tile prewarm: rendered z=%d x=%d y=%d in %s (%d bytes; %d done, %d cached/empty, %d failed)",
								z, x, y, time.Since(t0).Round(time.Millisecond), len(pngBytes), rendered, skipped, failed)
						}
					} else {
						failed++
						if logger != nil {
							logger.Warnf("tile prewarm: cache put z=%d x=%d y=%d failed", z, x, y)
						}
					}
				default:
					skipped++
					if logger != nil {
						logger.Debugf("tile prewarm: empty z=%d x=%d y=%d (%d bytes, not cached)", z, x, y, len(pngBytes))
					}
				}
				// Rate limit: at most one tile render started per second (the
				// render itself usually takes longer than this anyway).
				if !sleepCtx(ctx, prewarmMinInterval) {
					return
				}
			}
		}
	}
	if logger != nil {
		logger.Infof("tile prewarm: done — %d rendered, %d cached/empty, %d failed in %s",
			rendered, skipped, failed, time.Since(start).Round(time.Second))
	}
}

// prewarmMinCacheableTileBytes mirrors handleTile's threshold for "this PNG
// actually has content worth caching".
const prewarmMinCacheableTileBytes = 1024

// prewarmWaitIdle blocks until no handler request has arrived for
// prewarmIdleWindow, logging once when it starts holding so "the prewarmer is
// paused because someone is browsing" is visible. Returns false when ctx is
// cancelled.
func (h *ENCHandlers) prewarmWaitIdle(ctx context.Context, logger logging.Logger) bool {
	waited := false
	for {
		idleFor := h.sinceLastRequest()
		if idleFor >= prewarmIdleWindow {
			return true
		}
		if !waited && logger != nil {
			logger.Infof("tile prewarm: waiting for %s of request quiet (last request %s ago)",
				prewarmIdleWindow, idleFor.Round(time.Second))
		}
		waited = true
		wait := prewarmIdleWindow - idleFor
		if wait > prewarmIdlePollMax {
			wait = prewarmIdlePollMax
		}
		if !sleepCtx(ctx, wait) {
			return false
		}
	}
}

// sleepCtx sleeps for d, returning false early if ctx is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
