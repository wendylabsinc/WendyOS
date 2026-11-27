package commands

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

func TestDownloadImageWithoutTerminal(t *testing.T) {
	for _, tc := range []struct {
		name              string
		json, interactive bool
	}{
		{"json-even-with-terminal", true, true}, {"noninteractive", false, false}, {"json-noninteractive", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateVMImageCache(t)
			oldJSON, oldInteractive, oldTransport := jsonOutput, isInteractiveTerminalFn, http.DefaultTransport
			t.Cleanup(func() {
				jsonOutput = oldJSON
				isInteractiveTerminalFn = oldInteractive
				http.DefaultTransport = oldTransport
			})
			jsonOutput = tc.json
			isInteractiveTerminalFn = func() bool { return tc.interactive }
			http.DefaultTransport = vmImageTestTransport(func(req *http.Request) (*http.Response, error) {
				body := "image bytes"
				if req.Method == http.MethodHead {
					body = ""
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			})
			path, err := downloadImageContext(context.Background(), &imageInfo{DownloadURL: "https://image.test/headless"})
			if err != nil {
				t.Fatalf("download without TUI: %v", err)
			}
			defer os.Remove(path)
			data, err := os.ReadFile(path)
			if err != nil || string(data) != "image bytes" {
				t.Fatalf("download=%q, err=%v", data, err)
			}
		})
	}
}
