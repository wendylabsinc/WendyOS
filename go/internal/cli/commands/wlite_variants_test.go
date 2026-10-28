package commands

import "testing"

// The embedded catalog is maintained by pasting wendy-lite/catalog.json over
// the string literal, so this guards against a botched paste: it must parse,
// list at least one board, and every board must be fully populated.
func TestWendyLiteBoards(t *testing.T) {
	boards, err := WendyLiteBoards()
	if err != nil {
		t.Fatalf("WendyLiteBoards() error: %v", err)
	}
	if len(boards) == 0 {
		t.Fatal("WendyLiteBoards() returned no boards")
	}

	seen := make(map[string]bool)
	for _, b := range boards {
		if b.Board == "" || b.Target == "" || b.DisplayName == "" {
			t.Errorf("board %+v has an empty field", b)
		}
		if b.Version != "(latest)" {
			t.Errorf("board %s: Version = %q, want %q", b.Board, b.Version, "(latest)")
		}
		if seen[b.Board] {
			t.Errorf("duplicate board %q", b.Board)
		}
		seen[b.Board] = true
	}

	if !seen["esp32c6_net"] {
		t.Error("expected board esp32c6_net in the catalog")
	}
}

// TestWendyLiteTargets guards the same embedded-catalog paste as
// TestWendyLiteBoards, but for the catalog's targets array.
func TestWendyLiteTargets(t *testing.T) {
	targets, err := WendyLiteTargets()
	if err != nil {
		t.Fatalf("WendyLiteTargets() error: %v", err)
	}
	if len(targets) == 0 {
		t.Fatal("WendyLiteTargets() returned no targets")
	}

	seen := make(map[string]bool)
	for _, tg := range targets {
		if tg.Name == "" || tg.DisplayName == "" {
			t.Errorf("target %+v has an empty field", tg)
		}
		seen[tg.Name] = true
	}

	if !seen["esp32c6"] {
		t.Error("expected target esp32c6 in the catalog")
	}
}

// WendyLiteFirmwareID maps a catalog board to the key the GCS firmware manifests
// are published under: the board's binary name minus the wendy_mcu_ prefix.
func TestWendyLiteFirmwareID(t *testing.T) {
	cases := []struct {
		board string
		want  string
	}{
		{"esp32c6_net", "esp32c6_net"},
		{"esp32s31_mosaico_native", "esp32s31_mosaico_native"},
		{"esp32c6_generic_native", "esp32c6_native"},
		{"esp32c5_net_psram", "esp32c5_net_psram"},
	}
	for _, tc := range cases {
		got, err := WendyLiteFirmwareID(tc.board)
		if err != nil {
			t.Errorf("WendyLiteFirmwareID(%q) error: %v", tc.board, err)
			continue
		}
		if got != tc.want {
			t.Errorf("WendyLiteFirmwareID(%q) = %q, want %q", tc.board, got, tc.want)
		}
	}

	if _, err := WendyLiteFirmwareID("no_such_board"); err == nil {
		t.Error("WendyLiteFirmwareID(no_such_board) = nil error, want an error")
	}
}

func TestWendyLiteTargetForBoard(t *testing.T) {
	tests := map[string]string{
		"esp32c5_net_psram":         "esp32c5",
		"esp32c6_net":               "esp32c6",
		"esp32c61_generic_native":   "esp32c61",
		"esp32s31_generic_native":   "esp32s31",
		"esp32s3_seeed_xiao_native": "esp32s3",
	}
	for board, want := range tests {
		t.Run(board, func(t *testing.T) {
			got, err := WendyLiteTargetForBoard(board)
			if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Fatalf("WendyLiteTargetForBoard(%q) = %q, want %q", board, got, want)
			}
		})
	}
}

// wendyLiteBoardsWithFirmware drops catalog boards that have no published
// firmware in the manifest's Firmware map, so pickers never offer a board
// whose flash attempt would fail right after selection.
func TestWendyLiteBoardsWithFirmwareFilter(t *testing.T) {
	boards := []WendyLiteBoard{
		{Board: "esp32c61_generic_native", Target: "esp32c61", DisplayName: "no manifest entry"},
		{Board: "esp32c6_generic_native", Target: "esp32c6", DisplayName: "empty manifest path"},
		{Board: "esp32c5_net_psram", Target: "esp32c5", DisplayName: "stable only"},
		{Board: "esp32c6_net", Target: "esp32c6", DisplayName: "nightly only"},
		{Board: "no_such_board", Target: "esp32c6", DisplayName: "not in catalog"},
	}
	firmware := map[string]manifestDevice{
		// esp32c61_native: no entry at all.
		"esp32c6_native":    {ManifestPath: ""}, // entry present but unpublished.
		"esp32c5_net_psram": {ManifestPath: "firmware/esp32c5_net_psram.json", Latest: "0.2.0"},
		"esp32c6_net":       {ManifestPath: "firmware/esp32c6_net.json", LatestNightly: "0.3.0-nightly"},
	}

	stable := wendyLiteBoardsWithFirmware(boards, firmware, false)
	if len(stable) != 1 || stable[0].Board != "esp32c5_net_psram" {
		t.Errorf("nightly=false: got %+v, want only esp32c5_net_psram", stable)
	}

	nightly := wendyLiteBoardsWithFirmware(boards, firmware, true)
	if len(nightly) != 2 {
		t.Fatalf("nightly=true: got %+v, want esp32c5_net_psram and esp32c6_net", nightly)
	}
	seen := make(map[string]bool)
	for _, b := range nightly {
		seen[b.Board] = true
	}
	if !seen["esp32c5_net_psram"] || !seen["esp32c6_net"] {
		t.Errorf("nightly=true: got %+v, want esp32c5_net_psram and esp32c6_net", nightly)
	}
}
