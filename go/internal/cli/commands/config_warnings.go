package commands

import (
	"fmt"
	"io"
	"os"

	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func appConfigWarningsFromFile(cfgPath string) ([]string, error) {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("reading wendy.json: %w", err)
	}
	return appconfig.ValidateJSON(data), nil
}

func printAppConfigWarnings(w io.Writer, warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(w, "Warning: %s\n", warning)
	}
}

func warnAppConfigFile(cfgPath string) error {
	warnings, err := appConfigWarningsFromFile(cfgPath)
	if err != nil {
		return err
	}
	printAppConfigWarnings(os.Stderr, warnings)
	return nil
}
