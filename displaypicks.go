package vc

// The web app discovers the machine's resources itself (App.svelte
// updateResources) and reports what it picked to the nav service via the
// set_display_resources DoCommand. The display API (displayapi.go) uses
// those picks as fallbacks for anything the chartplotter config doesn't
// name — explicit config always wins — so thin display clients (the tvOS
// app) work on machines with no display-api config at all.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/services/navigation"
)

// DisplayPicks names the resources the web app picked for itself.
type DisplayPicks struct {
	MovementSensor string   `json:"movement_sensor,omitempty"`
	DepthSensor    string   `json:"depth_sensor,omitempty"`
	RouteSensor    string   `json:"route_sensor,omitempty"`
	Cameras        []string `json:"cameras,omitempty"`
}

func (p DisplayPicks) empty() bool {
	return p.MovementSensor == "" && p.DepthSensor == "" && p.RouteSensor == "" && len(p.Cameras) == 0
}

// The picks registry is package-level shared state: the writer (the nav
// service) and the reader (the chartplotter's display API) are separate
// resources in the same module process with no handle on each other.
var displayPicksReg struct {
	mu    sync.Mutex
	gen   int // bumped on every change; readers cache their resolution per gen
	picks DisplayPicks
	nav   navigation.Service // in-process handle of the nav service that reported
}

// displayPicksPath is where picks persist across module restarts, so a
// display client keeps working before any browser has (re)connected.
func displayPicksPath() string {
	return filepath.Join(resolveCacheRoot(""), "display-picks.json")
}

// setDisplayPicks records the web app's picks and the nav service that
// relayed them, then persists the picks best-effort.
func setDisplayPicks(p DisplayPicks, nav navigation.Service, path string, logger logging.Logger) {
	displayPicksReg.mu.Lock()
	displayPicksReg.picks = p
	displayPicksReg.nav = nav
	displayPicksReg.gen++
	displayPicksReg.mu.Unlock()
	data, err := json.Marshal(p)
	if err == nil {
		if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
			err = os.WriteFile(path, data, 0o644)
		}
	}
	if err != nil {
		logger.Warnf("display picks: persist to %s failed: %v", path, err)
	}
}

// loadDisplayPicks seeds the registry from disk. Only an untouched registry
// loads, so picks already reported this process are never clobbered.
func loadDisplayPicks(path string, logger logging.Logger) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // never saved (or unreadable) — nothing to seed
	}
	var p DisplayPicks
	if err := json.Unmarshal(data, &p); err != nil {
		logger.Warnf("display picks: bad %s: %v", path, err)
		return
	}
	if p.empty() {
		return
	}
	displayPicksReg.mu.Lock()
	defer displayPicksReg.mu.Unlock()
	if displayPicksReg.gen != 0 {
		return
	}
	displayPicksReg.picks = p
	displayPicksReg.gen = 1
	logger.Infof("display picks: loaded from %s (movement_sensor=%q, depth=%q, route=%q, %d cameras)",
		path, p.MovementSensor, p.DepthSensor, p.RouteSensor, len(p.Cameras))
}

// getDisplayPicks returns the current picks, the reporting nav service (nil
// until one reports in this process), and the generation counter.
func getDisplayPicks() (DisplayPicks, navigation.Service, int) {
	displayPicksReg.mu.Lock()
	defer displayPicksReg.mu.Unlock()
	return displayPicksReg.picks, displayPicksReg.nav, displayPicksReg.gen
}
