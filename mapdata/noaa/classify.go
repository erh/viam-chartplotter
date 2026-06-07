package noaa

// MinZoomForObjectClass returns the lowest map zoom at which an S-57 object
// class should render. Below this, the feature is dropped so coarse zooms
// aren't blanketed in symbols. Matches the spirit of S-52 scale-dependent
// symbology — NOAA's WMS thins out wrecks, obstructions, soundings, and
// minor navaids at zoomed-out scales for exactly this reason. Tuned by
// eyeballing the compare test against z=12/14/16 NOAA tiles.
//
// This is the single source of truth shared by the disk renderer (which
// drops out-of-zoom features at draw time) and the MongoDB ingest (which
// stores the value so a future Mongo-backed renderer can filter on it).
func MinZoomForObjectClass(class string) int {
	switch class {
	// Major area fills — always show.
	case "LNDARE", "DEPARE", "DRGARE", "BUAARE", "UNSARE", "LOKBSN":
		return 0
	// Single buildings (commercial / conspicuous structures): only at
	// chart-detail zoom.
	case "BUISGL":
		return 14
	// Coastline + depth contours — always.
	case "COALNE", "DEPCNT":
		return 0
	// Shoreline construction (piers, jetties, seawalls): hundreds per
	// harbour cell, way too dense at coastal zoom. Show only at chart
	// detail.
	case "SLCONS":
		return 15
	// Major navaids visible at overview. NOAA renders these at z=9
	// (sometimes z=8) so a sailor scanning a chart at coastal scale still
	// sees major lights, lateral marks, and major hazards.
	case "BOYLAT", "BCNLAT", "LIGHTS":
		return 9
	case "BOYCAR", "BOYISD", "BOYSAW", "BOYSPP", "BOYINB":
		return 11
	case "BCNCAR", "BCNISD", "BCNSAW", "BCNSPP":
		return 13
	// Topmarks attach to buoys/beacons and only make sense at chart-detail
	// zoom; smaller and they're indistinguishable from their parent symbol.
	case "TOPMAR":
		return 14
	case "DAYMAR":
		return 14
	// Hazards. Wrecks/obstructions are dense in harbour cells; only show
	// at chart-detail zoom. Underwater rocks even more so.
	case "WRECKS", "OBSTRN":
		return 15
	case "UWTROC":
		return 16
	// Soundings: NOAA renders depth labels at z=9 already (offshore tiles
	// show "65", "83", "95"-style depth labels). They're the densest
	// feature class, so dropping them at z<9 keeps the chart readable.
	case "SOUNDG":
		return 9
	// Mooring/pile/anchorage: harbour-detail zoom.
	case "MORFAC", "PILPNT", "MOORNG", "ACHBRT":
		return 15
	// Linear features.
	case "RIVERS", "BRIDGE", "CAUSWY":
		return 11
	// Overhead structures: cables, pipes, conveyors. The structures
	// vector layer kicks in at z >= 13 and is responsible for the
	// hover-able icon; below that the tile must draw the structure
	// itself, otherwise it would disappear off the chart between
	// coastal and harbour zoom. Same z=11 threshold as BRIDGE so all
	// four classes show up together when the vector layer is off.
	case "CBLOHD", "PIPOHD", "CONVYR":
		return 11
	// Channel limits / fairways / restricted areas — magenta lines show
	// at z=9 in NOAA charts (busy in our renders below that).
	case "FAIRWY", "RECTRC", "NAVLNE", "ACHARE", "DWRTPT", "TWRTPT", "RESARE":
		return 9
	case "PIPSOL", "CBLSUB":
		return 15
	case "DAMCON", "PONTON":
		return 14
	case "DOCARE", "HRBFAC", "HRBARE", "PIPARE":
		return 13
	}
	return 14
}
