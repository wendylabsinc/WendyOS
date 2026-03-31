package tui

import "errors"

// ErrCancelled is returned when the user cancels an interactive prompt (Ctrl+C).
var ErrCancelled = errors.New("cancelled")
