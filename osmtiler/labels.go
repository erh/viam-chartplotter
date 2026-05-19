package osmtiler

import (
	"fmt"
	"sync"

	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
)

// Label rendering is Latin-only for now: we use the bundled Go font
// (Bigelow & Holmes' Go Regular, ships with golang.org/x/image) and
// the freetype glyph rasteriser via gg. Non-Latin scripts that need
// HarfBuzz-style shaping (CJK, Arabic, Devanagari) render as boxes
// until v0.2b-step-2 adds go-text/typesetting.

var (
	labelFontOnce sync.Once
	labelFontTTF  *truetype.Font
	labelFontErr  error
)

func loadLabelFont() (*truetype.Font, error) {
	labelFontOnce.Do(func() {
		labelFontTTF, labelFontErr = truetype.Parse(goregular.TTF)
		if labelFontErr != nil {
			labelFontErr = fmt.Errorf("parse goregular: %w", labelFontErr)
		}
	})
	return labelFontTTF, labelFontErr
}

// labelFontFace returns a font.Face at the requested point size. New
// faces are cheap (per-call cost is bytes, not work), so callers can
// build one per render without caching.
func labelFontFace(size float64) (font.Face, error) {
	f, err := loadLabelFont()
	if err != nil {
		return nil, err
	}
	return truetype.NewFace(f, &truetype.Options{Size: size}), nil
}

// drawLabelWithHalo paints `text` centered at (x, y) with a 1px white
// halo behind a black fill. The halo is the same text rendered eight
// times at unit offsets, which is cheap (eight extra glyph blits per
// label) and gives osm-carto-like legibility against any background.
func drawLabelWithHalo(dc *gg.Context, x, y float64, text string) {
	dc.SetRGB(1, 1, 1)
	for _, dx := range [...]float64{-1, 0, 1} {
		for _, dy := range [...]float64{-1, 0, 1} {
			if dx == 0 && dy == 0 {
				continue
			}
			dc.DrawStringAnchored(text, x+dx, y+dy, 0.5, 0.5)
		}
	}
	dc.SetRGB(0, 0, 0)
	dc.DrawStringAnchored(text, x, y, 0.5, 0.5)
}

// labelSizeForClass returns the point size for this class's labels.
// Cities get larger text than POIs; sizes are loose approximations of
// osm-carto and will get class/zoom-aware refinement in v0.2c.
func labelSizeForClass(c Class) float64 {
	switch c {
	case ClassPlace:
		return 13
	case ClassPOI:
		return 10
	}
	return 11
}
