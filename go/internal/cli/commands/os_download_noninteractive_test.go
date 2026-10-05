package commands

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestDownloadArtifactWithoutTerminal(t *testing.T) {
	previous := jsonOutput
	jsonOutput = true
	t.Cleanup(func() { jsonOutput = previous })
	payload := bytes.Repeat([]byte("ordinary OTA download\n"), 8192)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(server.Close)
	path, err := downloadArtifactToTemp(server.URL + "/image.wendy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("returned before the complete artifact was written")
	}
	if !strings.HasSuffix(path, ".wendy") {
		t.Fatalf("artifact suffix lost: %s", path)
	}
}
