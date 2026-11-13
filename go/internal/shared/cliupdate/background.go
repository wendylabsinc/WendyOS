package cliupdate

import (
	"os"

	"github.com/wendylabsinc/wendy/go/internal/shared/env"
)

// BackgroundChecksDisabled suppresses ambient release checks and reminders.
// An explicit wendy update invocation remains available.
func BackgroundChecksDisabled() bool {
	return env.IsCI() || os.Getenv("WENDY_NO_UPDATE_CHECK") == "1" || os.Getenv("WENDY_NO_UPDATE_CHECK") == "true"
}
