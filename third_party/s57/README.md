# S-57 Parser for Go

[![Go Reference](https://pkg.go.dev/badge/github.com/beetlebugorg/s57.svg)](https://pkg.go.dev/github.com/beetlebugorg/s57)
[![CI](https://github.com/beetlebugorg/s57/actions/workflows/ci.yml/badge.svg)](https://github.com/beetlebugorg/s57/actions/workflows/ci.yml)
[![Documentation](https://img.shields.io/badge/docs-github.io-blue)](https://beetlebugorg.github.io/s57)

A pure Go parser for IHO S-57 Electronic Navigational Chart (ENC) files, the international standard for digital hydrographic data used worldwide in maritime navigation.

📖 **[Documentation](https://beetlebugorg.github.io/s57)** | 📦 **[Go Package](https://pkg.go.dev/github.com/beetlebugorg/s57)** | 🗺️ **[Examples](https://beetlebugorg.github.io/s57/docs/examples)**

## Overview

S-57 is the data transfer standard developed by the International Hydrographic Organization (IHO) for digital hydrographic data. This parser reads S-57 ENC datasets, extracting features, geometries, and metadata.

## About This Project

**Experimental software for learning and development only.** This project explores AI-assisted development with complex marine electronic navigational charts standards.

**⚠️ Not for production use or actual navigation.**

**Documentation as Specification:** The documentation serves as a living specification that drives development. We strive to ensure documented features are implemented and working, but we're actively refining our development and testing workflows. Some features may be in progress.

**Not affiliated with IHO (International Hydrographic Organization).** Independent third-party implementation of publicly available specifications.

**Useful for:**
- Learning how marine electronic navigational charts work
- Experimenting with S-57 chart parsing and feature extraction
- Understanding IHO S-57 format/specification
- Building proof-of-concept marine electronic navigational chart tools

## Features

- ✅ IHO S-57 Edition 3.1 parsing
- ✅ All S-57 feature types (DEPCNT, DEPARE, BOYCAR, etc.)
- ✅ Spatial topology support (isolated nodes, connected nodes, edges, faces)
- ✅ Geometry construction (points, line strings, polygons)
- ✅ Feature attributes extraction
- ✅ Dataset metadata (DSID) parsing
- ✅ Coordinate transformation (COMF/SOMF multiplication factors)
- ✅ Automatic update file merging (.001, .002, etc.)
- ✅ INSERT/DELETE/MODIFY update operations
- ✅ Filesystem abstraction (afero) for testing and custom storage
- ✅ Built on ISO 8211 parser
- ✅ Pure Go implementation

## Installation

```bash
go get github.com/beetlebugorg/s57
```

## Quick Start

```go
package main

import (
    "fmt"
    "log"

    "github.com/beetlebugorg/s57/pkg/s57"
)

func main() {
    // Create parser
    parser := s57.NewParser()

    // Parse an ENC file
    chart, err := parser.Parse("US5MA22M.000")
    if err != nil {
        log.Fatal(err)
    }

    // Access chart metadata
    fmt.Printf("Chart: %s (Edition %s)\\n", chart.DatasetName(), chart.Edition())
    fmt.Printf("Features: %d\\n", chart.FeatureCount())

    for _, feature := range chart.Features() {
        fmt.Printf("Feature ID=%d, Class=%s, Geometry=%s\\n",
            feature.ID,
            feature.ObjectClass,
            feature.Geometry.Type)

        // Access attributes
        if depth, ok := feature.Attributes["DRVAL1"]; ok {
            fmt.Printf("  Depth: %v meters\\n", depth)
        }
    }
}
```

## API Documentation

### Core Types

#### `Parser`
The main interface for parsing S-57 files.

```go
parser := s57.NewParser()
chart, err := parser.Parse("chart.000")
```

#### `Chart`
Container for parsed chart data with features and metadata.

```go
// Access features
features := chart.Features()

// Access metadata
name := chart.DatasetName()
edition := chart.Edition()
bounds := chart.Bounds()
```

#### `Feature`
Represents an S-57 feature object.

```go
type Feature struct {
    ID          int64
    ObjectClass string // e.g., "DEPCNT", "DEPARE", "BOYCAR"
    Geometry    Geometry
    Attributes  map[string]interface{}
}
```

#### `Geometry`
Spatial representation of a feature.

```go
type Geometry struct {
    Type        GeometryType // Point, LineString, Polygon
    Coordinates [][]float64  // [lon, lat] or [lon, lat, depth] for soundings
}
```

### Parse Options

Control parsing behavior with `ParseOptions`:

```go
opts := s57.ParseOptions{
    ApplyUpdates:        true,      // Apply update files (default: true)
    ValidateGeometry:    true,      // Validate all coordinates
    ObjectClassFilter:   []string{"DEPCNT", "DEPARE"}, // Only extract specific types
}

chart, err := parser.ParseWithOptions("chart.000", opts)
```

### Filesystem Abstraction

Use custom filesystems for testing or cloud storage with [afero](https://github.com/spf13/afero):

```go
import "github.com/spf13/afero"

// In-memory filesystem for testing
fs := afero.NewMemMapFs()
afero.WriteFile(fs, "/chart.000", chartData, 0644)
chart, err := s57.ParseFS(fs, "/chart.000")

// Or with full control
opts := s57.DefaultParseOptions()
opts.Fs = fs
chart, err := s57.ParseWithOptions("/chart.000", opts)
```

**Use cases:** In-memory testing, cloud storage (S3/GCS/Azure), embedded data, virtual filesystems.

**See:** [Filesystem Abstraction Example](docs/examples/11-filesystem-abstraction) for complete examples including update file handling.

## Update File Handling

S-57 charts are distributed as a base cell (.000) with optional updates (.001, .002, etc.). The parser automatically discovers and applies all sequential updates in the same directory.

### Automatic Update Application (Default)

```go
// Automatically finds and applies GB5X01SW.001, .002, etc.
chart, err := parser.Parse("GB5X01SW.000")
```

The parser will:
1. Discover all sequential update files (.001, .002, .003, etc.)
2. Stop at the first gap in the sequence
3. Apply updates in order before building geometries
4. Update metadata (update number, dates) to reflect the latest update

### Disable Update Application

```go
opts := s57.ParseOptions{
    ApplyUpdates: false, // Parse only base cell
}
chart, err := parser.ParseWithOptions("GB5X01SW.000", opts)
```

### Update Operations

Updates use the RUIN (Record Update Instruction) field per S-57 specification:

- **INSERT (1)**: Add new features or spatial records
- **DELETE (2)**: Remove existing records
- **MODIFY (3)**: Update existing records

The parser applies these operations at the record level before constructing geometries, ensuring all spatial topology is correctly maintained.

### Update Discovery

The parser looks for updates in sequence until the first gap:

**Example:**
- Base: `GB5X01SW.000`
- Finds: `GB5X01SW.001`, `GB5X01SW.002`, `GB5X01SW.003`
- Missing: `GB5X01SW.004`
- Stops: Parser applies .001, .002, .003 only

This ensures updates are always applied in the correct order.

## S-57 Structure

An S-57 ENC file consists of:

1. **Data Set Identification (DSID)** - Chart metadata, edition, dates
2. **Data Set Parameters (DSPM)** - Coordinate/sounding multiplication factors
3. **Feature Records** - Navigational objects with attributes
4. **Spatial Records** - Geometric primitives (nodes, edges, faces)

The parser automatically:
- Builds spatial topology from vector primitives
- Constructs complete geometries for each feature
- Applies coordinate transformations
- Links features to their spatial components

## File Format

S-57 files typically have extensions:
- `.000` - Base cell (full dataset)
- `.001`, `.002`, etc. - Update files

The parser automatically discovers and applies update files when parsing a base cell (see [Update File Handling](#update-file-handling) above).

## Dependencies

This parser requires:
- [github.com/beetlebugorg/iso8211](https://github.com/beetlebugorg/iso8211) - ISO 8211 file format parser
- [github.com/dhconnelly/rtreego](https://github.com/dhconnelly/rtreego) - R-tree for spatial indexing
- [github.com/spf13/afero](https://github.com/spf13/afero) - Filesystem abstraction for Go

## Testing

```bash
go test -v
go test -bench=.
go test -cover
```

## Standard Reference

This implementation follows:
- **IHO S-57 Edition 3.1** - IHO Transfer Standard for Digital Hydrographic Data
- **ISO/IEC 8211:1994** - Underlying file format

Key S-57 sections:
- §7.3: Record structure (DSID, DSPM, feature/spatial records)
- §7.6: Feature records and attributes
- §7.7: Spatial records and topology
- Appendix A: Object catalogue

## Acknowledgments

This parser uses the S-57 attribute catalogue CSV file from the [GDAL project](https://gdal.org/), licensed under MIT/X11. The attribute code to name mappings are embedded in the parser for automatic attribute name resolution.

## License

MIT License - see [LICENSE](LICENSE) file for details.

## Contributing

Contributions welcome! Please ensure:
- Tests pass: `go test ./...`
- Code is formatted: `go fmt ./...`
- Documentation is updated

## Resources

### Official Standards
- [IHO S-57 Edition 3.1 Main Specification](https://iho.int/uploads/user/pubs/standards/s-57/31Main.pdf)
- [S-57 Appendix A - Object Catalogue Chapter 1](https://iho.int/uploads/user/pubs/standards/s-57/31ApAch1.pdf)
- [S-57 Appendix A - Object Catalogue Chapter 2 (Attributes)](https://iho.int/uploads/user/pubs/standards/s-57/31ApAch2.pdf)
- [ISO/IEC 8211 Summary (IHO)](https://iho.int/uploads/user/Services%20and%20Standards/S-100WG/S-100WG7/S100WG7-4.16_2022_EN_ISO_IEC8211_Summary.pdf)
- [IHO Standards and Specifications](https://iho.int/en/standards-and-specifications)

### Documentation
- [API Documentation](https://beetlebugorg.github.io/s57/) - Complete usage guide with examples