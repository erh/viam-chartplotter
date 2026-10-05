package parser

// GeometryType represents the type of geometry for a feature
type GeometryType int

const (
	// GeometryTypePoint represents a single point location
	GeometryTypePoint GeometryType = iota
	// GeometryTypeLineString represents a line composed of connected points
	GeometryTypeLineString
	// GeometryTypePolygon represents a closed polygon area
	GeometryTypePolygon
)

// String returns the string representation of the geometry type
func (g GeometryType) String() string {
	switch g {
	case GeometryTypePoint:
		return "Point"
	case GeometryTypeLineString:
		return "LineString"
	case GeometryTypePolygon:
		return "Polygon"
	default:
		return "Unknown"
	}
}

// Geometry represents the spatial representation of a feature
// S-57 §7.3: Spatial record structure
type Geometry struct {
	// Type is the geometry type (Point, LineString, or Polygon)
	Type GeometryType
	// Coordinates is an array of [longitude, latitude] pairs
	// Per GeoJSON convention: [lon, lat]
	Coordinates [][]float64
}

// constructGeometry builds a Geometry from feature and spatial records
// S-57 §2.1: Features reference spatial records to build geometry
func constructGeometry(featureRec *featureRecord, spatialRecords map[spatialKey]*spatialRecord) (Geometry, error) {
	// PRIM=255 means N/A (no geometry) - these are meta-features like C_AGGR, M_COVR, etc.
	// Return empty point geometry for these
	if featureRec.GeomPrim == 255 {
		return Geometry{
			Type:        GeometryTypePoint,
			Coordinates: [][]float64{},
		}, nil
	}

	// If no spatial references, cannot construct geometry
	if len(featureRec.SpatialRefs) == 0 {
		return Geometry{}, &ErrMissingSpatialRecord{
			FeatureID: featureRec.ID,
			SpatialID: 0,
		}
	}

	// Determine geometry type from PRIM field (IHO S-57 §7.6.1)
	// PRIM: 1=Point, 2=Line, 3=Area, 255=N/A
	geomType := geomTypeFromPrim(featureRec.GeomPrim)

	// For polygon features (PRIM=3), use VRPT topology resolver
	if geomType == GeometryTypePolygon {
		return constructPolygonGeometry(featureRec, spatialRecords)
	}

	// For Point features (PRIM=1), use only the FIRST spatial ref
	// S-57 §7.6: Point features reference a single isolated node
	if geomType == GeometryTypePoint {
		return constructPointGeometry(featureRec, spatialRecords)
	}

	// For LineString features (PRIM=2), collect coordinates from all spatial refs
	// S-57 §7.6: Line features may reference edges (RCNM=130) which require topology resolution
	return constructLineStringGeometry(featureRec, spatialRecords)
}

// constructLineStringGeometry builds linestring geometry from spatial references
// S-57 §7.6: Line features reference edges (RCNM=130) or connected nodes
func constructLineStringGeometry(featureRec *featureRecord, spatialRecords map[spatialKey]*spatialRecord) (Geometry, error) {
	allCoords := make([][]float64, 0)
	resolver := newPolygonBuilder(spatialRecords)

	for _, spatialRef := range featureRec.SpatialRefs {
		// Find the spatial record - try all possible RCNMs since FSPT only gives RCID
		// S-57 spatial records can be: 110=isolated node, 120=connected node, 130=edge, 140=face
		spatial := lookupSpatialRef(spatialRef, spatialRecords,
			spatialTypeEdge, spatialTypeConnectedNode, spatialTypeIsolatedNode, spatialTypeFace)

		if spatial == nil {
			// Missing spatial record - skip gracefully
			continue
		}

		// If this is an edge (RCNM=130), use full edge resolution including nodes
		if spatial.RecordType == spatialTypeEdge {
			edge, err := resolver.loadEdge(spatial.ID)
			if err != nil {
				continue // Skip edges that can't be loaded
			}
			// Get full edge coordinates with nodes (use orientation from FSPT)
			edgeCoords := resolver.getFullEdgeCoordinates(edge, spatialRef.Orientation)
			for _, coord := range edgeCoords {
				allCoords = append(allCoords, []float64{coord[0], coord[1]})
			}
		} else if len(spatial.Coordinates) > 0 {
			// Direct coordinates from node
			for _, coord := range spatial.Coordinates {
				allCoords = append(allCoords, []float64{coord[0], coord[1]})
			}
		} else if len(spatial.VectorPointers) > 0 {
			// Follow VRPT pointers
			coordsFromPointers := resolveVectorPointers(spatial, spatialRecords)
			allCoords = append(allCoords, coordsFromPointers...)
		}
	}

	if len(allCoords) < 2 {
		// Not enough coordinates for a valid line
		// Return empty geometry (feature will be skipped by caller)
		return Geometry{
			Type:        GeometryTypeLineString,
			Coordinates: [][]float64{},
		}, nil
	}

	return Geometry{
		Type:        GeometryTypeLineString,
		Coordinates: allCoords,
	}, nil
}

// constructPointGeometry builds point geometry from spatial references
// S-57 §7.6: Point features can reference:
//   - Single isolated node (RCNM=110) for simple point features
//   - Multiple isolated nodes for multipoint features (e.g., SOUNDG with many soundings)
func constructPointGeometry(featureRec *featureRecord, spatialRecords map[spatialKey]*spatialRecord) (Geometry, error) {
	// Collect coordinates from ALL spatial references
	// For multipoint features like SOUNDG, there can be hundreds of refs
	allCoords := make([][]float64, 0)

	for _, spatialRef := range featureRec.SpatialRefs {
		// Try to find the spatial record - check isolated node first, then connected node
		// Point features can reference either RCNM=110 (isolated) or RCNM=120 (connected)
		// NOTE: Check isolated node FIRST - for multipoint features like SOUNDG,
		// the SG3D coordinates are stored in the isolated node, not the connected node
		spatial := lookupSpatialRef(spatialRef, spatialRecords,
			spatialTypeIsolatedNode, spatialTypeConnectedNode)

		if spatial == nil {
			// Skip missing spatial records (don't fail entire feature)
			continue
		}

		// Get coordinates from this spatial record
		if len(spatial.Coordinates) > 0 {
			// Extract ALL coordinates from spatial record
			// Preserve all dimensions (2D or 3D) - don't strip Z coordinates
			allCoords = append(allCoords, spatial.Coordinates...)
		}
	}

	if len(allCoords) == 0 {
		// All spatial refs were missing or had no coordinates
		// Return empty geometry (feature will be skipped by caller)
		return Geometry{
			Type:        GeometryTypePoint,
			Coordinates: [][]float64{},
		}, nil
	}

	return Geometry{
		Type:        GeometryTypePoint,
		Coordinates: allCoords,
	}, nil
}

// constructPolygonGeometry builds polygon geometry using VRPT topology resolution
// S-57 §7.3: Area features use VRPT to reference edge topology
func constructPolygonGeometry(featureRec *featureRecord, spatialRecords map[spatialKey]*spatialRecord) (Geometry, error) {
	// Create polygon builder
	resolver := newPolygonBuilder(spatialRecords)

	// Check if feature references face records (spatial primitives with VRPT)
	// Collect edge references WITH orientation from FSPT
	edgeRefs := make([]spatialRef, 0)
	for _, fsptRef := range featureRec.SpatialRefs {
		// FSPT references can be to any spatial type - try all RCNMs to find by RCID
		spatial := lookupSpatialRef(fsptRef, spatialRecords,
			spatialTypeFace, spatialTypeEdge, spatialTypeConnectedNode, spatialTypeIsolatedNode)

		if spatial == nil {
			continue
		}

		// If this is a face record (RCNM=140), collect edge references from VRPT
		if spatial.RecordType == spatialTypeFace {
			for _, ptr := range spatial.VectorPointers {
				// Edge records have RCNM=130
				if ptr.TargetRCNM == int(spatialTypeEdge) {
					// Face VRPT has orientation - use it
					edgeRefs = append(edgeRefs, spatialRef{
						RCNM:        ptr.TargetRCNM,
						RCID:        ptr.TargetRCID,
						Orientation: ptr.Orientation,
						Usage:       ptr.Usage,
						Mask:        ptr.Mask,
					})
				}
			}
		} else if spatial.RecordType == spatialTypeEdge {
			// Direct edge reference - use FSPT orientation
			edgeRefs = append(edgeRefs, fsptRef)
		}
	}

	// If we have edge references, resolve topology
	if len(edgeRefs) > 0 {
		rings, err := resolver.resolvePolygon(edgeRefs)
		if err != nil {
			// VRPT resolution failed - fall back to direct coordinate collection
			// This can happen if topology is incomplete or malformed (e.g., M_COVR meta features)
			// Try to collect coordinates directly from edges
			allCoords := make([][]float64, 0)
			for _, edgeRef := range edgeRefs {
				edgeKey := spatialKey{RCNM: int(spatialTypeEdge), RCID: edgeRef.RCID}
				if edge, ok := spatialRecords[edgeKey]; ok && len(edge.Coordinates) > 0 {
					for _, coord := range edge.Coordinates {
						allCoords = append(allCoords, []float64{coord[0], coord[1]})
					}
				}
			}

			if len(allCoords) > 0 {
				allCoords = ensurePolygonClosure(allCoords)
				return Geometry{
					Type:        GeometryTypePolygon,
					Coordinates: allCoords,
				}, nil
			}

			// If we still can't get coordinates from edges, try collecting from ANY spatial record
			// This handles cases where the feature references spatial records that aren't properly linked
			for _, spatialRef := range featureRec.SpatialRefs {
				for key, spatial := range spatialRecords {
					if refMatches(spatialRef, key) && len(spatial.Coordinates) > 0 {
						for _, coord := range spatial.Coordinates {
							allCoords = append(allCoords, []float64{coord[0], coord[1]})
						}
					}
				}
			}

			if len(allCoords) > 0 {
				allCoords = ensurePolygonClosure(allCoords)
				return Geometry{
					Type:        GeometryTypePolygon,
					Coordinates: allCoords,
				}, nil
			}

			// Last resort: return the error
			return Geometry{}, err
		}

		// Convert rings to coordinate format
		allCoords := make([][]float64, 0)
		for _, ring := range rings {
			for _, point := range ring {
				allCoords = append(allCoords, []float64{point[0], point[1]})
			}
		}

		// Check if we have enough coordinates for a valid polygon
		if len(allCoords) < 3 {
			// Degenerate polygon - return empty geometry
			return Geometry{
				Type:        GeometryTypePolygon,
				Coordinates: [][]float64{},
			}, nil
		}

		return Geometry{
			Type:        GeometryTypePolygon,
			Coordinates: allCoords,
		}, nil
	}

	// Fallback: No VRPT topology, collect direct coordinates
	allCoords := make([][]float64, 0)
	for _, spatialRef := range featureRec.SpatialRefs {
		// Search by RCID
		for key, spatial := range spatialRecords {
			if refMatches(spatialRef, key) && len(spatial.Coordinates) > 0 {
				for _, coord := range spatial.Coordinates {
					allCoords = append(allCoords, []float64{coord[0], coord[1]})
				}
			}
		}
	}

	// Check if we have enough coordinates for a valid polygon
	if len(allCoords) < 3 {
		// Degenerate polygon - return empty geometry
		return Geometry{
			Type:        GeometryTypePolygon,
			Coordinates: [][]float64{},
		}, nil
	}

	// Ensure polygon closure
	allCoords = ensurePolygonClosure(allCoords)

	return Geometry{
		Type:        GeometryTypePolygon,
		Coordinates: allCoords,
	}, nil
}

// geomTypeFromPrim converts PRIM value to GeometryType
// Per IHO S-57 §7.6.1: PRIM values are 1=Point, 2=Line, 3=Area, 255=N/A
func geomTypeFromPrim(prim int) GeometryType {
	switch prim {
	case 1: // Point
		return GeometryTypePoint
	case 2: // Line
		return GeometryTypeLineString
	case 3: // Area
		return GeometryTypePolygon
	default: // 255 = N/A or unknown
		return GeometryTypePoint // Default to point if unknown
	}
}

// ensurePolygonClosure ensures a polygon is closed (first coordinate == last)
func ensurePolygonClosure(coords [][]float64) [][]float64 {
	if len(coords) < 3 {
		return coords // Not enough points for polygon
	}

	// Check if already closed
	first := coords[0]
	last := coords[len(coords)-1]

	if len(first) == 2 && len(last) == 2 {
		if first[0] == last[0] && first[1] == last[1] {
			return coords // Already closed
		}
	}

	// Add closing point
	closed := make([][]float64, len(coords)+1)
	copy(closed, coords)
	closed[len(coords)] = []float64{first[0], first[1]}

	return closed
}

// resolveVectorPointers recursively resolves VRPT pointers to collect coordinates
func resolveVectorPointers(spatial *spatialRecord, spatialRecords map[spatialKey]*spatialRecord) [][]float64 {
	visited := make(map[int64]bool)
	return resolveVectorPointersRecursive(spatial, spatialRecords, visited)
}

// resolveVectorPointersRecursive implements recursive VRPT resolution with cycle detection
func resolveVectorPointersRecursive(spatial *spatialRecord, spatialRecords map[spatialKey]*spatialRecord, visited map[int64]bool) [][]float64 {
	coords := make([][]float64, 0)

	for _, ptr := range spatial.VectorPointers {
		// Check for circular reference
		if visited[ptr.TargetRCID] {
			continue // Skip to prevent infinite loop
		}
		visited[ptr.TargetRCID] = true

		// Lookup using composite key (RCNM, RCID)
		targetKey := spatialKey{RCNM: ptr.TargetRCNM, RCID: ptr.TargetRCID}
		target, ok := spatialRecords[targetKey]
		if !ok {
			continue // Target not found, skip
		}

		// Collect coordinates from target
		targetCoords := make([][]float64, 0)
		if len(target.Coordinates) > 0 {
			// Target has direct coordinates
			for _, coord := range target.Coordinates {
				targetCoords = append(targetCoords, []float64{coord[0], coord[1]})
			}
		} else if len(target.VectorPointers) > 0 {
			// Target has no direct coords, recurse
			targetCoords = resolveVectorPointersRecursive(target, spatialRecords, visited)
		}

		// Apply orientation (reverse if needed)
		if ptr.Orientation == 2 { // Reverse
			for i := len(targetCoords) - 1; i >= 0; i-- {
				coords = append(coords, targetCoords[i])
			}
		} else { // Forward or null
			coords = append(coords, targetCoords...)
		}
	}

	return coords
}

// isSpatialRCNM reports whether rcnm names a vector record type.
func isSpatialRCNM(rcnm int) bool {
	switch spatialType(rcnm) {
	case spatialTypeIsolatedNode, spatialTypeConnectedNode, spatialTypeEdge, spatialTypeFace:
		return true
	}
	return false
}

// lookupSpatialRef resolves a feature-to-spatial pointer. RCIDs are unique
// only within a record type (S-57 §2.2.2), so when the pointer carries its
// RCNM (FSPT NAME always does) only that exact (RCNM, RCID) record matches:
// guessing the type would resolve a pointer to a deleted edge to an unrelated
// node that happens to share its RCID. The guess order is used only for refs
// without a valid RCNM.
func lookupSpatialRef(ref spatialRef, records map[spatialKey]*spatialRecord, guess ...spatialType) *spatialRecord {
	if isSpatialRCNM(ref.RCNM) {
		return records[spatialKey{RCNM: ref.RCNM, RCID: ref.RCID}]
	}
	for _, t := range guess {
		if sp, ok := records[spatialKey{RCNM: int(t), RCID: ref.RCID}]; ok {
			return sp
		}
	}
	return nil
}

// refMatches reports whether a spatial record key is the target of ref,
// honouring the ref's RCNM when it has one.
func refMatches(ref spatialRef, key spatialKey) bool {
	if key.RCID != ref.RCID {
		return false
	}
	return !isSpatialRCNM(ref.RCNM) || key.RCNM == ref.RCNM
}
