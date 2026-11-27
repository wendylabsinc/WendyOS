//go:build darwin || linux || windows

package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

type liteCompanionClient interface {
	HostedCompanionBuildID(context.Context) (string, error)
	PushHostedCompanion(context.Context, string, func(written, total uint32)) error
}

func installLiteCompanion(ctx context.Context, port discovery.SerialPortInfo, interactive bool, companion *firmwareCompanion) (discovery.SerialPortInfo, error) {
	open := func(port discovery.SerialPortInfo) (discovery.SerialPortInfo, *liteclient.WendyLiteClient, error) {
		ready, err := waitInstalledLiteUSB(ctx, port, interactive, "companion update", "update")
		if err != nil {
			return ready, nil, err
		}
		client := liteclient.NewWendyLiteClient()
		connectCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := client.ConnectToSerialContext(connectCtx, ready.Port); err != nil {
			client.Close()
			return ready, nil, err
		}
		return ready, client, nil
	}

	installNote(os.Stdout, "Checking the ESP-Hosted companion firmware...")
	port, client, err := open(port)
	if err != nil {
		return port, err
	}
	defer client.Close()
	updateCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	updated, err := updateLiteCompanion(updateCtx, os.Stdout, client, companion, gcsBaseURL+"/"+companion.Path)
	if err != nil || !updated {
		return port, err
	}

	// The companion reboots after activation. Restart the P4 to re-establish
	// the Hosted transport, as in Espressif's OTA example.
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
		return port, ctx.Err()
	}
	if err := client.ResetTargetDevice(true, 0); err != nil {
		return port, fmt.Errorf("restarting P4 after companion activation: %w", err)
	}
	client.WaitReboot(ctx)
	if err := client.Close(); err != nil {
		return port, err
	}

	port, verified, err := open(port)
	if err != nil {
		return port, err
	}
	defer verified.Close()
	buildID, err := readLiteCompanionBuildID(ctx, verified)
	if err != nil {
		return port, fmt.Errorf("reading updated companion build ID: %w", err)
	}
	if buildID != companion.BuildID {
		return port, fmt.Errorf("companion booted build %q, expected %q", buildID, companion.BuildID)
	}
	installNote(os.Stdout, "ESP-Hosted companion firmware updated to %s.", buildID)
	return port, nil
}

func updateLiteCompanion(ctx context.Context, out io.Writer, client liteCompanionClient, companion *firmwareCompanion, downloadURL string) (bool, error) {
	buildID, err := readLiteCompanionBuildID(ctx, client)
	if err != nil {
		return false, fmt.Errorf("reading companion build ID over USB: %w", err)
	}
	if buildID == companion.BuildID {
		installNote(out, "ESP-Hosted companion firmware already matches %s.", buildID)
		return false, nil
	}

	installNote(out, "Updating the ESP-Hosted companion to %s...", companion.BuildID)
	path, err := downloadLiteCompanion(ctx, downloadURL, companion)
	if err != nil {
		return false, err
	}
	defer os.Remove(path)
	if err := client.PushHostedCompanion(ctx, path, nil); err != nil {
		return false, fmt.Errorf("applying companion OTA: %w", err)
	}
	return true, nil
}

func readLiteCompanionBuildID(ctx context.Context, client liteCompanionClient) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		id, err := client.HostedCompanionBuildID(ctx)
		if !errors.Is(err, liteclient.ErrHostedCompanionUnavailable) {
			return id, err
		}
		// WendyCom can accept USB connections before ESP-Hosted comes up.
		select {
		case <-time.After(500 * time.Millisecond):
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %w", err, ctx.Err())
		}
	}
}

func downloadLiteCompanion(ctx context.Context, downloadURL string, companion *firmwareCompanion) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading companion firmware: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("companion download returned status %d", resp.StatusCode)
	}
	file, err := os.CreateTemp("", "wendy-companion-*.bin")
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, companion.SizeBytes+1))
	closeErr := file.Close()
	switch {
	case copyErr != nil:
		err = copyErr
	case closeErr != nil:
		err = closeErr
	case size != companion.SizeBytes:
		err = fmt.Errorf("companion download has size %d, expected %d", size, companion.SizeBytes)
	case !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), companion.Checksum):
		err = fmt.Errorf("companion firmware checksum mismatch")
	}
	if err != nil {
		os.Remove(file.Name())
		return "", err
	}
	return file.Name(), nil
}
