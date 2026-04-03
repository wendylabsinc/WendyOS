package tui

import "errors"

// ErrCancelled is returned when the user cancels an interactive prompt (e.g. Ctrl+C or q).
var ErrCancelled = errors.New("cancelled")
