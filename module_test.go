package vc

import "testing"

func TestResolveTileServerBaseURL(t *testing.T) {
	for _, tc := range []struct {
		name, explicit, mongoURI, want string
	}{
		{"explicit wins with mongo", "http://tiles:8989", "mongodb://db", "http://tiles:8989"},
		{"explicit wins without mongo", "http://tiles:8989", "", "http://tiles:8989"},
		{"no mongo falls back to hosted", "", "", DefaultHostedTileServer},
		{"mongo set serves same-origin", "", "mongodb://db", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveTileServerBaseURL(tc.explicit, tc.mongoURI); got != tc.want {
				t.Fatalf("ResolveTileServerBaseURL(%q, %q) = %q, want %q", tc.explicit, tc.mongoURI, got, tc.want)
			}
		})
	}
}
