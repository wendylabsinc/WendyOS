package commands

import (
	"context"
	"os"
	"path/filepath"
)

type tourPresentationKey struct{}

func isRobotTour(ctx context.Context) bool {
	return ctx.Value(tourPresentationKey{}) == true
}

func tourRobotResumeCommand(projectPath string) string {
	return tourResumeCommand(filepath.Dir(projectPath))
}

func tourResumeCommand(projectDir string) string {
	command := "wendy tour --simulator"
	if projectDir == "" {
		return command
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(projectDir) == filepath.Join(home, "Documents", "wendy-robot-tour") {
		return command
	}
	return command + " --project " + shellQuoteArg(projectDir)
}

func suppressTourProgressHints() func() {
	previous, exists := os.LookupEnv("WENDY_NO_PROGRESS_HINTS")
	_ = os.Setenv("WENDY_NO_PROGRESS_HINTS", "1")
	return func() {
		if exists {
			_ = os.Setenv("WENDY_NO_PROGRESS_HINTS", previous)
		} else {
			_ = os.Unsetenv("WENDY_NO_PROGRESS_HINTS")
		}
	}
}
