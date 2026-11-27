//go:build darwin || linux || windows

package commands

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

const testedCompanionBuildID = "wendy-0123456789abcdef01234567"

type testLiteCompanion struct {
	buildID    string
	readErrors []error
	pushErr    error
	pushed     []byte
}

func (c *testLiteCompanion) HostedCompanionBuildID(context.Context) (string, error) {
	if len(c.readErrors) > 0 {
		err := c.readErrors[0]
		c.readErrors = c.readErrors[1:]
		return "", err
	}
	return c.buildID, nil
}

func (c *testLiteCompanion) PushHostedCompanion(_ context.Context, path string, _ func(uint32, uint32)) error {
	var err error
	c.pushed, err = os.ReadFile(path)
	if err != nil {
		return err
	}
	return c.pushErr
}

func TestLiteCompanionUpdatesOnlyOnMismatch(t *testing.T) {
	image := "C6 OTA image"
	companion := &firmwareCompanion{BuildID: testedCompanionBuildID, SizeBytes: int64(len(image)), Checksum: fmt.Sprintf("%x", sha256.Sum256([]byte(image)))}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, image)
	}))
	defer server.Close()

	device := &testLiteCompanion{buildID: testedCompanionBuildID}
	if changed, err := updateLiteCompanion(t.Context(), io.Discard, device, companion, server.URL); changed || err != nil {
		t.Fatalf("matching companion: changed=%v, error=%v", changed, err)
	}
	if requests.Load() != 0 || device.pushed != nil {
		t.Fatal("matching companion fetched or wrote firmware")
	}

	device.buildID = "2.13.1"
	if changed, err := updateLiteCompanion(t.Context(), io.Discard, device, companion, server.URL); !changed || err != nil {
		t.Fatalf("factory companion: changed=%v, error=%v", changed, err)
	}
	if requests.Load() != 1 || string(device.pushed) != image {
		t.Fatalf("requests=%d, pushed=%q", requests.Load(), device.pushed)
	}

	device.pushErr = errors.New("radio disconnected")
	if changed, err := updateLiteCompanion(t.Context(), io.Discard, device, companion, server.URL); changed || !errors.Is(err, device.pushErr) {
		t.Fatalf("interrupted OTA: changed=%v, error=%v", changed, err)
	}
}

func TestLiteCompanionRetriesLostStatusReply(t *testing.T) {
	device := &testLiteCompanion{
		buildID:    testedCompanionBuildID,
		readErrors: []error{context.DeadlineExceeded},
	}
	id, err := readLiteCompanionBuildID(t.Context(), device)
	if err != nil || id != testedCompanionBuildID {
		t.Fatalf("status after lost reply: ID=%q, error=%v", id, err)
	}
}

func TestLiteCompanionRejectsUnverifiedDownload(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "wrong")
	}))
	defer server.Close()
	companion := &firmwareCompanion{BuildID: testedCompanionBuildID, SizeBytes: 5, Checksum: strings.Repeat("0", 64)}
	device := &testLiteCompanion{}
	if changed, err := updateLiteCompanion(t.Context(), io.Discard, device, companion, server.URL); changed || err == nil {
		t.Fatalf("corrupt artifact: changed=%v, error=%v", changed, err)
	}
	if device.pushed != nil {
		t.Fatal("corrupt artifact reached OTA")
	}
}

func TestLiteFirmwareManifestCompanionIsOptInPerVersion(t *testing.T) {
	var manifest firmwareManifest
	raw := fmt.Sprintf(`{"versions":{"1.0.0":{"path":"firmware/p4/1.0.0/host.bin"},"1.1.0":{"path":"firmware/p4/1.1.0/host.bin","companion":{"build_id":%q,"path":"firmware/p4/1.1.0/companion.bin","checksum":%q,"size_bytes":3}}}}`, testedCompanionBuildID, strings.Repeat("0", 64))
	if err := json.Unmarshal([]byte(raw), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Versions["1.0.0"].Companion != nil {
		t.Fatal("legacy version requested a companion")
	}
	entry := manifest.Versions["1.1.0"]
	if entry.Companion == nil {
		t.Fatal("explicit companion metadata was lost")
	}
	if err := entry.Companion.validate(entry.Path); err != nil {
		t.Fatal(err)
	}
	entry.Companion.Path = "firmware/p4/1.0.0/companion.bin"
	if err := entry.Companion.validate(entry.Path); err == nil {
		t.Fatal("accepted companion path from a different release")
	}
}
