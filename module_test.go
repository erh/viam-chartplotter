package vc

import (
	"testing"

	"go.viam.com/rdk/components/generic"
)

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

func TestMDNSInstanceName(t *testing.T) {
	for _, tc := range []struct {
		name, hostname, resName, want string
	}{
		// macOS hostnames carry ".local"; verbatim they'd put a dot in
		// the instance name, which mDNSResponder browsers silently drop.
		{"macos hostname stripped", "cm40-mac1.local", "chartplotter", "cm40-mac1"},
		{"linux hostname unchanged", "cm40-ul1", "chartplotter", "cm40-ul1"},
		{"non-default resource appended", "cm40-ul1", "nav2", "cm40-ul1 nav2"},
		{"macos plus resource", "cm40-mac1.local", "salon", "cm40-mac1 salon"},
		{"dotted hostname sanitized", "foo.bar.local", "chartplotter", "foo-bar"},
		{"no hostname falls back", "", "chartplotter", "chartplotter"},
		{"no hostname uses resource", "", "salon", "salon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mdnsInstanceName(tc.hostname, generic.Named(tc.resName)); got != tc.want {
				t.Fatalf("mdnsInstanceName(%q, %q) = %q, want %q", tc.hostname, tc.resName, got, tc.want)
			}
		})
	}
}
