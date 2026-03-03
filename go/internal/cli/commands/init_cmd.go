package commands

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/internal/shared/appconfig"
)

func newInitCmd() *cobra.Command {
	var language string
	var template string

	cmd := &cobra.Command{
		Use:   "init [app-id]",
		Short: "Initialize a new Wendy project",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return fmt.Errorf("getting working directory: %w", err)
			}

			appID := filepath.Base(cwd)
			if len(args) > 0 {
				appID = args[0]
			}

			// Check if wendy.json already exists.
			cfgPath := filepath.Join(cwd, "wendy.json")
			if _, err := os.Stat(cfgPath); err == nil {
				return fmt.Errorf("wendy.json already exists in %s", cwd)
			}

			// Build default entitlements based on template/language.
			entitlements := defaultEntitlements(language, template)

			cfg := appconfig.AppConfig{
				AppID:        appID,
				Version:      "0.1.0",
				Language:     language,
				Entitlements: entitlements,
			}

			data, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				return fmt.Errorf("marshaling config: %w", err)
			}

			if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
				return fmt.Errorf("writing wendy.json: %w", err)
			}

			fmt.Printf("Created wendy.json for %s\n", appID)

			// Create language-specific scaffolding.
			switch language {
			case "python":
				if err := initPythonProject(cwd, appID, template); err != nil {
					return err
				}
			case "swift":
				if err := initSwiftProject(cwd, appID, template); err != nil {
					return err
				}
			default:
				if err := initDockerProject(cwd, appID); err != nil {
					return err
				}
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&language, "language", "", "Project language: python, swift")
	cmd.Flags().StringVar(&template, "template", "", "Project template: voice-assistant, speech-to-text, basic")

	return cmd
}

// defaultEntitlements returns sensible default entitlements based on language and template.
func defaultEntitlements(language, template string) []appconfig.Entitlement {
	entitlements := []appconfig.Entitlement{
		{Type: appconfig.EntitlementNetwork},
	}

	switch template {
	case "voice-assistant":
		entitlements = append(entitlements,
			appconfig.Entitlement{Type: appconfig.EntitlementAudio},
			appconfig.Entitlement{Type: appconfig.EntitlementGPU},
			appconfig.Entitlement{Type: appconfig.EntitlementBluetooth},
		)
	case "speech-to-text":
		entitlements = append(entitlements,
			appconfig.Entitlement{Type: appconfig.EntitlementAudio},
			appconfig.Entitlement{Type: appconfig.EntitlementGPU},
		)
	default:
		// For Python AI apps, include GPU by default.
		if language == "python" {
			entitlements = append(entitlements,
				appconfig.Entitlement{Type: appconfig.EntitlementGPU},
			)
		}
	}

	return entitlements
}

func initPythonProject(dir, appID, template string) error {
	// Create app.py.
	appPath := filepath.Join(dir, "app.py")
	if _, err := os.Stat(appPath); err == nil {
		// Already exists, skip.
	} else {
		content := pythonAppContent(appID, template)
		if err := os.WriteFile(appPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("creating app.py: %w", err)
		}
	}

	// Create requirements.txt.
	reqPath := filepath.Join(dir, "requirements.txt")
	if _, err := os.Stat(reqPath); err == nil {
		// Already exists, skip.
	} else {
		content := pythonRequirements(template)
		if err := os.WriteFile(reqPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("creating requirements.txt: %w", err)
		}
	}

	// Create Dockerfile.
	dockerPath := filepath.Join(dir, "Dockerfile")
	if _, err := os.Stat(dockerPath); err == nil {
		// Already exists, skip.
	} else {
		content := pythonDockerfileContent(appID)
		if err := os.WriteFile(dockerPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("creating Dockerfile: %w", err)
		}
	}

	fmt.Println("Created app.py, requirements.txt, and Dockerfile")
	return nil
}

func pythonAppContent(appID, template string) string {
	switch template {
	case "voice-assistant":
		return fmt.Sprintf(`"""%s - Voice Assistant Edge Application"""

import os
import signal
import sys


def signal_handler(sig, frame):
    print("Shutting down gracefully...")
    sys.exit(0)


def main():
    signal.signal(signal.SIGINT, signal_handler)
    signal.signal(signal.SIGTERM, signal_handler)

    print("Starting %s voice assistant...")
    print("Listening for wake word...")

    # Your voice assistant logic here.
    # Example: use a speech recognition library to listen for commands.
    try:
        while True:
            pass  # Replace with your voice processing loop.
    except KeyboardInterrupt:
        print("Stopped.")


if __name__ == "__main__":
    main()
`, appID, appID)
	case "speech-to-text":
		return fmt.Sprintf(`"""%s - Speech-to-Text Edge Application"""

import os
import signal
import sys


def signal_handler(sig, frame):
    print("Shutting down gracefully...")
    sys.exit(0)


def main():
    signal.signal(signal.SIGINT, signal_handler)
    signal.signal(signal.SIGTERM, signal_handler)

    print("Starting %s speech-to-text service...")

    # Your speech-to-text logic here.
    # Example: use whisper or vosk for on-device transcription.
    try:
        while True:
            pass  # Replace with your transcription loop.
    except KeyboardInterrupt:
        print("Stopped.")


if __name__ == "__main__":
    main()
`, appID, appID)
	default:
		return fmt.Sprintf(`"""%s - A Wendy Edge Application"""

import os
import signal
import sys


def signal_handler(sig, frame):
    print("Shutting down gracefully...")
    sys.exit(0)


def main():
    signal.signal(signal.SIGINT, signal_handler)
    signal.signal(signal.SIGTERM, signal_handler)

    print("Hello from %s!")


if __name__ == "__main__":
    main()
`, appID, appID)
	}
}

func pythonRequirements(template string) string {
	switch template {
	case "voice-assistant":
		return "# Voice assistant dependencies\n# Add your dependencies here, e.g.:\n# pvporcupine\n# pyaudio\n# openai\n"
	case "speech-to-text":
		return "# Speech-to-text dependencies\n# Add your dependencies here, e.g.:\n# faster-whisper\n# numpy\n"
	default:
		return "# Add your Python dependencies here\n"
	}
}

func pythonDockerfileContent(appID string) string {
	return fmt.Sprintf(`# Multi-stage build for %s
# Stage 1: Install dependencies
FROM python:3.11-slim AS builder
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir --target=/app/deps -r requirements.txt

# Stage 2: Runtime
FROM python:3.11-slim
WORKDIR /app

# Copy installed dependencies from builder
COPY --from=builder /app/deps /usr/local/lib/python3.11/site-packages/

# Copy application code
COPY . .

CMD ["python", "app.py"]
`, appID)
}

func initSwiftProject(dir, appID, template string) error {
	_ = template

	// Create Package.swift.
	pkgPath := filepath.Join(dir, "Package.swift")
	if _, err := os.Stat(pkgPath); err == nil {
		return nil
	}

	content := fmt.Sprintf(`// swift-tools-version:6.0
import PackageDescription

let package = Package(
    name: "%s",
    targets: [
        .executableTarget(name: "%s"),
    ]
)
`, appID, appID)

	if err := os.WriteFile(pkgPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("creating Package.swift: %w", err)
	}

	// Create source directory and main.swift.
	srcDir := filepath.Join(dir, "Sources", appID)
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		return fmt.Errorf("creating source directory: %w", err)
	}

	mainContent := fmt.Sprintf("print(\"Hello from %s!\")\n", appID)
	if err := os.WriteFile(filepath.Join(srcDir, "main.swift"), []byte(mainContent), 0o644); err != nil {
		return fmt.Errorf("creating main.swift: %w", err)
	}

	fmt.Println("Created Package.swift and source files")
	return nil
}

func initDockerProject(dir, appID string) error {
	dockerPath := filepath.Join(dir, "Dockerfile")
	if _, err := os.Stat(dockerPath); err == nil {
		return nil
	}

	content := fmt.Sprintf(`FROM ubuntu:22.04
WORKDIR /app
# Add your application here
CMD ["echo", "Hello from %s!"]
`, appID)

	if err := os.WriteFile(dockerPath, []byte(content), 0o644); err != nil {
		return fmt.Errorf("creating Dockerfile: %w", err)
	}

	fmt.Println("Created Dockerfile")
	return nil
}
