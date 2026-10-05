# Vendored fork of github.com/beetlebugorg/s57 v0.100.0

Wired in with `replace github.com/beetlebugorg/s57 => ./third_party/s57` in the
root `go.mod`. Upstream's `test/` data (4 MB) and `docs/` are left out, so the
tests that read `../../test/US4MD81M` fail here; copy that directory back from
the module cache to run them.

Changes from upstream, all in how `.001+` update files are merged (each one
corrupted shoreline geometry in real NOAA cells, e.g. US5NYCEG, US5PHLBB):

1. **Positional update control.** `SGCC`, `VRPC` and `FSPC` (insert, delete
   or modify N items at index i) are honoured. Upstream replaced the whole
   coordinate or pointer list with the update's partial one.
2. **Feature updates keyed by FRID RCID.** S-57 §8.4.2 addresses a record by
   its record name; NOAA DELETE records carry no `FOID`. Keying on FOID turned
   those deletes into no-ops.
3. **Attribute merge.** An `ATTF` in a MODIFY changes only the attributes it
   names, and the value `0x7F` deletes one. Upstream replaced the whole map.
4. **RCNM-exact spatial lookup.** `FSPT` pointers keep their RCNM, and
   geometry resolves the exact `(RCNM, RCID)`. Upstream guessed the type, so a
   pointer to a deleted edge resolved to an unrelated node with the same RCID.

Tests: `internal/parser/updates_control_test.go`. Worth sending upstream.
