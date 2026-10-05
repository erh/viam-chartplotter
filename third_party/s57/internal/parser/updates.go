package parser

import (
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/beetlebugorg/iso8211/pkg/iso8211"
	"github.com/spf13/afero"
)

// UpdateInstruction represents the RUIN (Record Update Instruction) field values
// S-57 Part 3 §8.4.2.2 and §8.4.3.2
type UpdateInstruction int

const (
	// UpdateInsert indicates a record should be inserted (RUIN = 1)
	UpdateInsert UpdateInstruction = 1

	// UpdateDelete indicates a record should be deleted (RUIN = 2)
	UpdateDelete UpdateInstruction = 2

	// UpdateModify indicates a record should be modified (RUIN = 3)
	UpdateModify UpdateInstruction = 3
)

// findUpdateFiles discovers sequential update files for a base cell
//
// Given "GB5X01SW.000", looks for "GB5X01SW.001", "GB5X01SW.002", etc.
// in the same directory. Returns paths in order.
func findUpdateFiles(fs afero.Fs, baseFilename string) ([]string, error) {
	// Get base filename without extension
	dir := filepath.Dir(baseFilename)
	base := filepath.Base(baseFilename)

	// Remove extension (.000)
	baseName := strings.TrimSuffix(base, filepath.Ext(base))

	var updates []string

	// Look for sequential updates: .001, .002, .003, etc.
	for updateNum := 1; updateNum <= 999; updateNum++ {
		updateFile := filepath.Join(dir, fmt.Sprintf("%s.%03d", baseName, updateNum))

		// Check if file exists using the provided filesystem
		exists, err := afero.Exists(fs, updateFile)
		if err != nil {
			return nil, fmt.Errorf("error checking for update file %s: %w", updateFile, err)
		}
		if exists {
			updates = append(updates, updateFile)
		} else {
			// Stop at first missing update (updates must be sequential)
			break
		}
	}

	return updates, nil
}

// applyUpdates applies update files to parsed chart data
//
// Updates are applied at the record level before geometry construction.
// This modifies featureRecords and spatialRecords in place.
func applyUpdates(fs afero.Fs, baseChart *chartData, updateFiles []string, params datasetParams) error {
	for _, updateFile := range updateFiles {
		if err := applyUpdate(fs, baseChart, updateFile, params); err != nil {
			return fmt.Errorf("failed to apply update %s: %w", updateFile, err)
		}
	}
	return nil
}

// chartData holds the intermediate chart state during update merging
type chartData struct {
	features       []*featureRecord
	spatialRecords map[spatialKey]*spatialRecord
	metadata       *datasetMetadata

	// Index for update lookup, keyed by the FRID record ID (RCID). Per S-57
	// §8.4.2 an update targets a feature record by its record name (RCNM+RCID),
	// not by FOID: DELETE records typically carry FRID only, with no FOID.
	featuresByRCID map[int64]*featureRecord
}

// applyUpdate applies a single update file to the chart data
func applyUpdate(fs afero.Fs, chart *chartData, updateFile string, params datasetParams) error {
	// Open update file from filesystem using OpenFS
	parser, err := iso8211.OpenFS(fs, updateFile)
	if err != nil {
		return fmt.Errorf("failed to open update file: %w", err)
	}
	defer parser.Close()

	isoFile, err := parser.Parse()
	if err != nil {
		return fmt.Errorf("failed to parse update file: %w", err)
	}

	// Process each record in update file
	for _, record := range isoFile.Records {
		// Feature record (FRID)
		if fridData, ok := record.Fields["FRID"]; ok && len(fridData) >= 12 {
			if err := applyFeatureUpdate(chart, record, fridData); err != nil {
				return err
			}
			continue
		}

		// Spatial record (VRID)
		if vridData, ok := record.Fields["VRID"]; ok && len(vridData) >= 8 {
			if err := applySpatialUpdate(chart, record, vridData, params); err != nil {
				return err
			}
			continue
		}
	}

	// Check if update contains new DSID metadata and merge it
	if updatedDSID := extractDSID(isoFile); updatedDSID != nil {
		// Merge updated metadata fields
		// Per S-57 spec, update files can modify UPDN (update number) and UADT (update date)
		// EDTN (edition) and DSNM (dataset name) should NOT change in updates
		if updatedDSID.updn != "" {
			chart.metadata.updn = updatedDSID.updn
		}
		if updatedDSID.uadt != "" {
			chart.metadata.uadt = updatedDSID.uadt
		}
		// Update issue date if present
		if updatedDSID.isdt != "" {
			chart.metadata.isdt = updatedDSID.isdt
		}
	}

	return nil
}

// applyFeatureUpdate handles INSERT/DELETE/MODIFY for features
func applyFeatureUpdate(chart *chartData, record *iso8211.DataRecord, fridData []byte) error {
	ruin := UpdateInstruction(fridData[11])

	// Parse feature record
	featureRec := parseFeatureRecord(record)
	if featureRec == nil {
		return fmt.Errorf("failed to parse feature record")
	}

	key := featureRec.RCID

	switch ruin {
	case UpdateInsert:
		// Add or replace feature
		// Note: Some ENC producers use INSERT even when the record exists in the base
		// This is treated as an upsert operation
		if existing, exists := chart.featuresByRCID[key]; exists {
			// Replace existing feature
			*existing = *featureRec
		} else {
			// Add new feature
			chart.features = append(chart.features, featureRec)
			chart.featuresByRCID[key] = featureRec
		}

	case UpdateDelete:
		// Remove existing feature
		existing, exists := chart.featuresByRCID[key]
		if !exists {
			// Feature doesn't exist - this is a no-op
			// This can happen if the base cell doesn't have the feature being deleted
			return nil
		}

		// Remove from index
		delete(chart.featuresByRCID, key)

		// Remove from slice
		for i, f := range chart.features {
			if f == existing {
				chart.features = append(chart.features[:i], chart.features[i+1:]...)
				break
			}
		}

	case UpdateModify:
		// Update existing feature
		existing, exists := chart.featuresByRCID[key]
		if !exists {
			return fmt.Errorf("MODIFY: feature record %d not found", key)
		}

		// Merge update record into existing feature
		// Per S-57 §8.4.2.2: MODIFY only updates fields present in the update record
		// We must selectively update fields rather than wholesale replacement

		// Always update these core identification fields
		existing.RecordVersion = featureRec.RecordVersion
		existing.UpdateInstr = featureRec.UpdateInstr

		// Per S-57 §8.4.2.2.a: an ATTF in a MODIFY carries only the attributes
		// that change. Each one replaces the existing value; a value of the
		// delete character (0x7F) removes the attribute. Attributes not named
		// in the update are kept.
		if _, hasATTF := record.Fields["ATTF"]; hasATTF {
			mergeAttributes(existing, featureRec.Attributes)
		}

		// Per S-57 §8.4.2.2.b: FSPT changes are positional, driven by FSPC
		// (insert/delete/modify N pointers at index i). Replacing the whole list
		// with the update's partial one wires the feature to the wrong edges.
		if fspcData, hasFSPC := record.Fields["FSPC"]; hasFSPC {
			ctl, ok := parseUpdateControl(fspcData)
			if !ok {
				return fmt.Errorf("MODIFY: feature record %d: short FSPC", key)
			}
			refs, err := applyControl(existing.SpatialRefs, featureRec.SpatialRefs, ctl)
			if err != nil {
				return fmt.Errorf("MODIFY: feature record %d FSPT: %w", key, err)
			}
			existing.SpatialRefs = refs
		} else if _, hasFSPT := record.Fields["FSPT"]; hasFSPT {
			// FSPT without FSPC is malformed; treat it as a full replacement,
			// the best reading available.
			existing.SpatialRefs = featureRec.SpatialRefs
		}

		// Keep reference in index
		chart.featuresByRCID[key] = existing

	default:
		return fmt.Errorf("unknown RUIN value for feature: %d", ruin)
	}

	return nil
}

// applySpatialUpdate handles INSERT/DELETE/MODIFY for spatial records
func applySpatialUpdate(chart *chartData, record *iso8211.DataRecord, vridData []byte, params datasetParams) error {
	ruin := UpdateInstruction(vridData[7])

	// Parse spatial record
	spatialRec := parseSpatialRecordWithParams(record, params)
	if spatialRec == nil {
		return fmt.Errorf("failed to parse spatial record")
	}

	// Build key from record type and ID
	key := spatialKey{
		RCNM: int(spatialRec.RecordType),
		RCID: spatialRec.ID,
	}

	switch ruin {
	case UpdateInsert:
		// Add or replace spatial record
		// Note: Some ENC producers use INSERT even when the record exists in the base
		// This is treated as an upsert operation
		chart.spatialRecords[key] = spatialRec

	case UpdateDelete:
		// Remove existing spatial record
		if _, exists := chart.spatialRecords[key]; !exists {
			// Record doesn't exist - this is a no-op
			return nil
		}
		delete(chart.spatialRecords, key)

	case UpdateModify:
		// Update existing spatial record
		existing, exists := chart.spatialRecords[key]
		if !exists {
			return fmt.Errorf("MODIFY: spatial record %v not found", key)
		}

		// Per S-57 §8.4.3.2: MODIFY only updates fields present in the update record
		// We must selectively merge fields rather than wholesale replacement
		// This is critical - update records may omit fields that should be preserved!

		// Always update core identification fields
		existing.RecordVersion = spatialRec.RecordVersion
		existing.UpdateInstr = spatialRec.UpdateInstr

		// Per S-57 §8.4.3.2.b: coordinate changes are positional, driven by
		// SGCC (insert/delete/modify N coordinates at index i). A DELETE carries
		// SGCC with no SG2D/SG3D at all.
		_, hasSG2D := record.Fields["SG2D"]
		_, hasSG3D := record.Fields["SG3D"]
		if sgccData, hasSGCC := record.Fields["SGCC"]; hasSGCC {
			ctl, ok := parseUpdateControl(sgccData)
			if !ok {
				return fmt.Errorf("MODIFY: spatial record %v: short SGCC", key)
			}
			coords, err := applyControl(existing.Coordinates, spatialRec.Coordinates, ctl)
			if err != nil {
				return fmt.Errorf("MODIFY: spatial record %v coordinates: %w", key, err)
			}
			existing.Coordinates = coords
		} else if hasSG2D || hasSG3D {
			// Coordinates without SGCC is malformed; treat as a full replacement.
			existing.Coordinates = spatialRec.Coordinates
		}

		// Per S-57 §8.4.3.2.a: VRPT changes are positional, driven by VRPC.
		if vrpcData, hasVRPC := record.Fields["VRPC"]; hasVRPC {
			ctl, ok := parseUpdateControl(vrpcData)
			if !ok {
				return fmt.Errorf("MODIFY: spatial record %v: short VRPC", key)
			}
			ptrs, err := applyControl(existing.VectorPointers, spatialRec.VectorPointers, ctl)
			if err != nil {
				return fmt.Errorf("MODIFY: spatial record %v pointers: %w", key, err)
			}
			existing.VectorPointers = ptrs
		} else if _, hasVRPT := record.Fields["VRPT"]; hasVRPT {
			// VRPT without VRPC is malformed; treat as a full replacement.
			existing.VectorPointers = spatialRec.VectorPointers
		}

		// Keep existing record in map (already there, but make it explicit)
		chart.spatialRecords[key] = existing

	default:
		return fmt.Errorf("unknown RUIN value for spatial: %d", ruin)
	}

	return nil
}

// updateControl is a decoded S-57 update control field: FSPC, VRPC or SGCC.
// All three share one layout (S-57 §7.6.9, §7.7.1.5, §7.7.1.7):
//
//	*UI b11  update instruction: 1=insert, 2=delete, 3=modify
//	*IX b12  1-based index of the first pointer/coordinate affected
//	*N  b12  number of pointers/coordinates affected
type updateControl struct {
	Instr UpdateInstruction
	Index int
	Count int
}

func parseUpdateControl(data []byte) (updateControl, bool) {
	if len(data) < 5 {
		return updateControl{}, false
	}
	return updateControl{
		Instr: UpdateInstruction(data[0]),
		Index: int(binary.LittleEndian.Uint16(data[1:3])),
		Count: int(binary.LittleEndian.Uint16(data[3:5])),
	}, true
}

// applyControl applies one positional update to a list (FSPT pointers, VRPT
// pointers, or SG2D/SG3D coordinates) per S-57 §8.4.2.2.b / §8.4.3.2:
//
//   - insert: the update's items go in before position Index (Index = len+1
//     appends).
//   - delete: Count items starting at Index are removed; the update carries
//     no items.
//   - modify: Count items starting at Index are replaced, in order, by the
//     update's items.
//
// It returns a new slice and never aliases the update's items into existing.
func applyControl[T any](existing, items []T, ctl updateControl) ([]T, error) {
	start := ctl.Index - 1
	switch ctl.Instr {
	case UpdateInsert:
		if start < 0 || start > len(existing) {
			return nil, fmt.Errorf("insert at index %d out of range (len %d)", ctl.Index, len(existing))
		}
		out := make([]T, 0, len(existing)+len(items))
		out = append(out, existing[:start]...)
		out = append(out, items...)
		return append(out, existing[start:]...), nil

	case UpdateDelete:
		if start < 0 || ctl.Count < 0 || start+ctl.Count > len(existing) {
			return nil, fmt.Errorf("delete %d at index %d out of range (len %d)", ctl.Count, ctl.Index, len(existing))
		}
		out := make([]T, 0, len(existing)-ctl.Count)
		out = append(out, existing[:start]...)
		return append(out, existing[start+ctl.Count:]...), nil

	case UpdateModify:
		if start < 0 || start+len(items) > len(existing) {
			return nil, fmt.Errorf("modify %d at index %d out of range (len %d)", len(items), ctl.Index, len(existing))
		}
		if len(items) != ctl.Count {
			return nil, fmt.Errorf("modify count %d but %d items in update", ctl.Count, len(items))
		}
		out := append([]T(nil), existing...)
		copy(out[start:], items)
		return out, nil
	}
	return nil, fmt.Errorf("unknown update instruction %d", ctl.Instr)
}

// attrDeleteValue is the S-57 "delete attribute" marker (§8.4.2.2.a): an
// update ATTF value consisting only of the delete character 0x7F.
const attrDeleteValue = "\x7f"

// mergeAttributes folds an update's ATTF into a feature's attributes: each
// attribute present replaces the existing value, and the 0x7F marker deletes
// it.
func mergeAttributes(existing *featureRecord, updates map[string]interface{}) {
	if existing.Attributes == nil {
		existing.Attributes = make(map[string]interface{}, len(updates))
	}
	for k, v := range updates {
		if s, ok := v.(string); ok && s == attrDeleteValue {
			delete(existing.Attributes, k)
			continue
		}
		existing.Attributes[k] = v
	}
}
