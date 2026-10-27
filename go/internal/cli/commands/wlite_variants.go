package commands

import (
	"encoding/json"
	"fmt"
	"strings"
)

// wendyLiteCatalogJSON is copied verbatim from wendy-lite/catalog.json.
// To update the variant list, paste the current file content between the
// backticks — nothing else to touch.
const wendyLiteCatalogJSON = `
{
    "version": 2,
    "binaries": [
        {
            "name": "esp32c5",
            "target": "esp32c5",
            "board_cfg": "esp32c5_generic",
            "flash_size": "4MB"
        },
        {
            "name": "esp32c5_native",
            "target": "esp32c5",
            "board_cfg": "esp32c5_generic_native",
            "flash_size": "4MB"
        },
        {
            "name": "esp32c6",
            "target": "esp32c6",
            "board_cfg": "esp32c6_generic",
            "flash_size": "4MB"
        },
        {
            "name": "esp32c6_native",
            "target": "esp32c6",
            "board_cfg": "esp32c6_generic_native",
            "flash_size": "4MB"
        },
        {
            "name": "esp32c61_native",
            "target": "esp32c61",
            "board_cfg": "esp32c61_generic_native",
            "flash_size": "4MB"
        },
        {
            "name": "esp32p4_waveshare_lcd_4b",
            "target": "esp32p4",
            "board_cfg": "waveshare_lcd_4b",
            "flash_size": "32MB"
        },
        {
            "name": "esp32p4_dfr1172_firebeetle",
            "target": "esp32p4",
            "board_cfg": "dfr1172_firebeetle",
            "flash_size": "16MB"
        },
        {
            "name": "esp32s3",
            "target": "esp32s3",
            "board_cfg": "esp32s3_generic",
            "flash_size": "4MB"
        },
        {
            "name": "esp32s3_native",
            "target": "esp32s3",
            "board_cfg": "esp32s3_generic_native",
            "flash_size": "4MB"
        },
        {
            "name": "esp32s3_seeed_xiao_native",
            "target": "esp32s3",
            "board_cfg": "seeed_xiao_esp32s3_native",
            "flash_size": "8MB"
        },
        {
            "name": "esp32s3_m5_stamp_s3_native",
            "target": "esp32s3",
            "board_cfg": "m5_stamp_s3_native",
            "flash_size": "8MB"
        },
        {
            "name": "esp32s31_mosaico_native",
            "target": "esp32s31",
            "board_cfg": "esp32s31_mosaico_native",
            "flash_size": "16MB",
            "idf_image": "espressif/idf@sha256:30b42a38415e918b1b14825ccf0e4c40e7910c5132618dcd12e3c45361be0943",
            "preview": true,
            "project_dir": "samples/mosaico_dashboard"
        }
    ],
    "targets": [
        {
            "name": "esp32c5",
            "display_name": "ESP32-C5"
        },
        {
            "name": "esp32c6",
            "display_name": "ESP32-C6"
        },
        {
            "name": "esp32c61",
            "display_name": "ESP32-C61"
        },
        {
            "name": "esp32p4",
            "display_name": "ESP32-P4"
        },
        {
            "name": "esp32s3",
            "display_name": "ESP32-S3"
        },
        {
            "name": "esp32s31",
            "display_name": "ESP32-S31 (preview)"
        }
    ],
    "boards": [
        {
            "name": "esp32c5_generic",
            "display_name": "Generic ESP32-C5 board, 4MB flash",
            "binary": "esp32c5",
            "target": "esp32c5"
        },
        {
            "name": "esp32c5_generic_native",
            "display_name": "Generic ESP32-C5 board, 4MB flash, native app support",
            "binary": "esp32c5_native",
            "target": "esp32c5"
        },
        {
            "name": "esp32c6_generic",
            "display_name": "Generic ESP32-C6 board, 4MB flash",
            "binary": "esp32c6",
            "target": "esp32c6"
        },
        {
            "name": "esp32c6_generic_native",
            "display_name": "Generic ESP32-C6 board, 4MB flash, native app support",
            "binary": "esp32c6_native",
            "target": "esp32c6"
        },
        {
            "name": "esp32c61_generic_native",
            "display_name": "Generic ESP32-C61 board, 4MB flash, native app support",
            "binary": "esp32c61_native",
            "target": "esp32c61"
        },
        {
            "name": "esp32p4_waveshare_lcd_4b",
            "display_name": "Waveshare ESP32-P4-WIFI6-Touch-LCD-4B, 32MB flash",
            "binary": "esp32p4_waveshare_lcd_4b",
            "target": "esp32p4"
        },
        {
            "name": "esp32p4_dfr1172_firebeetle",
            "display_name": "DFRobot FireBeetle 2 ESP32-P4 (DFR1172), 16MB flash",
            "binary": "esp32p4_dfr1172_firebeetle",
            "target": "esp32p4"
        },
        {
            "name": "esp32s3_generic",
            "display_name": "Generic ESP32-S3 board, 4MB flash",
            "binary": "esp32s3",
            "target": "esp32s3"
        },
        {
            "name": "esp32s3_generic_native",
            "display_name": "Generic ESP32-S3 board, 4MB flash, native app support",
            "binary": "esp32s3_native",
            "target": "esp32s3"
        },
        {
            "name": "esp32s3_seeed_xiao_native",
            "display_name": "Seeed Studio XIAO ESP32S3, 8MB flash, 8MB PSRAM, native app support",
            "binary": "esp32s3_seeed_xiao_native",
            "target": "esp32s3"
        },
        {
            "name": "esp32s3_m5_stamp_s3_native",
            "display_name": "M5Stack StampS3, 8MB flash, native app support",
            "binary": "esp32s3_m5_stamp_s3_native",
            "target": "esp32s3"
        },
        {
            "name": "esp32s31_mosaico_native",
            "display_name": "ESP-Mosaico, 16MB flash, touch display, mesh dashboard, preview",
            "binary": "esp32s31_mosaico_native",
            "target": "esp32s31"
        }
    ]
}
`

// WendyLiteBoard is one installable Wendy Lite flavor: a board paired with
// the firmware binary built for it and the ESP32 target chip it runs on.
type WendyLiteBoard struct {
	Board       string
	Target      string
	DisplayName string
	Version     string
}

// wendyLiteCatalogBoard is one entry of the embedded catalog's boards array.
type wendyLiteCatalogBoard struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Binary      string `json:"binary"`
	Target      string `json:"target"`
}

// wendyLiteCatalogBoards parses the embedded catalog and returns its boards
// in catalog order.
func wendyLiteCatalogBoards() ([]wendyLiteCatalogBoard, error) {
	var catalog struct {
		Boards []wendyLiteCatalogBoard `json:"boards"`
	}
	if err := json.Unmarshal([]byte(wendyLiteCatalogJSON), &catalog); err != nil {
		return nil, fmt.Errorf("parsing embedded Wendy Lite catalog: %w", err)
	}
	return catalog.Boards, nil
}

// WendyLiteBoards returns the Wendy Lite boards from the embedded catalog, in
// catalog order. The catalog carries no version information yet, so Version
// is always "(latest)".
func WendyLiteBoards() ([]WendyLiteBoard, error) {
	catalogBoards, err := wendyLiteCatalogBoards()
	if err != nil {
		return nil, err
	}

	boards := make([]WendyLiteBoard, 0, len(catalogBoards))
	for _, b := range catalogBoards {
		boards = append(boards, WendyLiteBoard{
			Board:       b.Name,
			Target:      b.Target,
			DisplayName: b.DisplayName,
			Version:     "(latest)",
		})
	}
	return boards, nil
}

// WendyLiteTarget is one ESP32 chip family the Wendy Lite catalog builds
// firmware for (e.g. "esp32c6").
type WendyLiteTarget struct {
	Name        string
	DisplayName string
}

// wendyLiteCatalogTarget is one entry of the embedded catalog's targets array.
type wendyLiteCatalogTarget struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

// wendyLiteCatalogTargets parses the embedded catalog and returns its targets
// in catalog order.
func wendyLiteCatalogTargets() ([]wendyLiteCatalogTarget, error) {
	var catalog struct {
		Targets []wendyLiteCatalogTarget `json:"targets"`
	}
	if err := json.Unmarshal([]byte(wendyLiteCatalogJSON), &catalog); err != nil {
		return nil, fmt.Errorf("parsing embedded Wendy Lite catalog: %w", err)
	}
	return catalog.Targets, nil
}

// WendyLiteTargets returns the ESP32 chip targets from the embedded catalog,
// in catalog order.
func WendyLiteTargets() ([]WendyLiteTarget, error) {
	catalogTargets, err := wendyLiteCatalogTargets()
	if err != nil {
		return nil, err
	}

	targets := make([]WendyLiteTarget, 0, len(catalogTargets))
	for _, t := range catalogTargets {
		targets = append(targets, WendyLiteTarget{
			Name:        t.Name,
			DisplayName: t.DisplayName,
		})
	}
	return targets, nil
}

// WendyLiteFirmwareID returns the firmware ID for a catalog board: its binary
// name without the "wendy_mcu_" prefix. Firmware IDs are the keys the GCS
// firmware manifests are published under.
func WendyLiteFirmwareID(board string) (string, error) {
	boards, err := wendyLiteCatalogBoards()
	if err != nil {
		return "", err
	}
	for _, b := range boards {
		if b.Name == board {
			return strings.TrimPrefix(b.Binary, "wendy_mcu_"), nil
		}
	}
	return "", fmt.Errorf("board %q not found in the Wendy Lite catalog", board)
}

// WendyLiteTargetForBoard returns the ESP32 target associated with a catalog
// board. Keeping this lookup in the catalog layer ensures firmware selection
// and the pre-flash chip-model check are derived from the same board record.
func WendyLiteTargetForBoard(board string) (string, error) {
	boards, err := wendyLiteCatalogBoards()
	if err != nil {
		return "", err
	}
	for _, b := range boards {
		if b.Name == board {
			return b.Target, nil
		}
	}
	return "", fmt.Errorf("board %q not found in the Wendy Lite catalog", board)
}

// wendyLiteBoardsWithFirmware filters boards down to those with a published
// firmware version in the main manifest's Firmware map, keyed by firmware ID.
// nightly selects which channel must have a build, matching how Linux
// devices are filtered in runOSInstall: nightly falls back to the stable
// version when no nightly build is published.
func wendyLiteBoardsWithFirmware(boards []WendyLiteBoard, firmware map[string]manifestDevice, nightly bool) []WendyLiteBoard {
	available := make([]WendyLiteBoard, 0, len(boards))
	for _, b := range boards {
		firmwareID, err := WendyLiteFirmwareID(b.Board)
		if err != nil {
			continue
		}
		chip, ok := firmware[firmwareID]
		if !ok || chip.ManifestPath == "" {
			continue
		}
		version := chip.Latest
		if nightly && chip.LatestNightly != "" {
			version = chip.LatestNightly
		}
		if version == "" {
			continue
		}
		available = append(available, b)
	}
	return available
}

// WendyLiteBoardsWithFirmware returns the catalog boards that currently have
// published firmware, so pickers never offer a board whose flash attempt
// would fail immediately after selection.
func WendyLiteBoardsWithFirmware(nightly bool) ([]WendyLiteBoard, error) {
	boards, err := WendyLiteBoards()
	if err != nil {
		return nil, err
	}
	main, err := fetchMainManifest()
	if err != nil {
		return nil, err
	}
	return wendyLiteBoardsWithFirmware(boards, main.Firmware, nightly), nil
}
