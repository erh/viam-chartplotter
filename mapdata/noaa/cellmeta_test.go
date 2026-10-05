package noaa

import "testing"

func TestCellMetaIsCurrent(t *testing.T) {
	cases := []struct {
		name            string
		meta            CellMeta
		edition, update int
		want            bool
	}{
		{"current", CellMeta{Edition: "17", UpdateNumber: "3", ParserVersion: ParserVersion}, 17, 3, true},
		{"new update", CellMeta{Edition: "17", UpdateNumber: "2", ParserVersion: ParserVersion}, 17, 3, false},
		{"new edition", CellMeta{Edition: "16", UpdateNumber: "0", ParserVersion: ParserVersion}, 17, 0, false},
		// Ingested before ParserVersion existed: cells with updates were merged
		// wrongly and must re-parse; base-only cells parsed the same.
		{"old parser, updates", CellMeta{Edition: "17", UpdateNumber: "3"}, 17, 3, false},
		{"old parser, base only", CellMeta{Edition: "17", UpdateNumber: "0"}, 17, 0, true},
	}
	for _, c := range cases {
		if got := c.meta.IsCurrent(c.edition, c.update); got != c.want {
			t.Errorf("%s: IsCurrent(%d, %d) = %v, want %v", c.name, c.edition, c.update, got, c.want)
		}
	}
}
