package commands

import "github.com/wendylabsinc/wendy-go/lite"

type WendyLiteBoard = lite.WendyLiteBoard
type WendyLiteTarget = lite.WendyLiteTarget

func WendyLiteBoards() ([]WendyLiteBoard, error)       { return lite.WendyLiteBoards() }
func WendyLiteTargets() ([]WendyLiteTarget, error)     { return lite.WendyLiteTargets() }
func WendyLiteFirmwareID(board string) (string, error) { return lite.WendyLiteFirmwareID(board) }
func WendyLiteTargetForBoard(board string) (string, error) {
	return lite.WendyLiteTargetForBoard(board)
}
func wendyLiteBoardsWithFirmware(boards []WendyLiteBoard, firmware map[string]manifestDevice, nightly bool) []WendyLiteBoard {
	published := make(map[string]lite.Firmware, len(firmware))
	for id, f := range firmware {
		published[id] = lite.Firmware{ManifestPath: f.ManifestPath, Latest: f.Latest, LatestNightly: f.LatestNightly}
	}
	return lite.BoardsWithFirmware(boards, published, nightly)
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
