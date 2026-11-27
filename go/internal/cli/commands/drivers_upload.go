package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

// prepareDriverUpload keeps direct downloads for connected devices and streams
// registry artifacts over the management connection for isolated USB devices.
// The caller owns cleanup and must keep it until the upload has finished.
func prepareDriverUpload(ctx context.Context, conn *grpcclient.AgentConnection, spec *agentpbv2.DriverSpec) (*agentpbv2.DriverSpec, string, func(), error) {
	noop := func() {}
	if spec.GetArtifactUrl() == "" || conn.AgentService == nil {
		return spec, "", noop, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	version, err := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
	cancel()
	if ctx.Err() != nil {
		return nil, "", noop, ctx.Err()
	}
	// A failed probe leaves connectivity unknown, as it does for OS updates.
	if err != nil || !shouldServeOSArtifactLocally(ctx, conn, version) {
		return spec, "", noop, nil
	}
	cliLogln("Device reports no WiFi or routable network address; downloading driver to upload...")
	path, err := downloadDriver(ctx, spec, "")
	if err != nil {
		return nil, "", noop, err
	}
	upload := proto.Clone(spec).(*agentpbv2.DriverSpec)
	upload.ArtifactUrl = ""
	return upload, path, func() { _ = os.Remove(path) }, nil
}

// downloadDriver checks the published digest before any bytes reach the device.
// The agent still verifies the digest, signature, and embedded kernel itself.
// dir is passed to os.CreateTemp; "" uses the default temp directory.
func downloadDriver(ctx context.Context, spec *agentpbv2.DriverSpec, dir string) (string, error) {
	digest, err := hex.DecodeString(spec.GetSha256())
	if err != nil || len(digest) != sha256.Size {
		return "", fmt.Errorf("driver %q has no valid SHA-256 in the manifest", spec.GetName())
	}
	f, err := os.CreateTemp(dir, "wendy-driver-*.raw")
	if err != nil {
		return "", err
	}
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(f.Name())
		}
	}()
	// No deadline on the transfer: it follows imageDownloadPolicy, like an OS image.
	if err := downloadResumable(ctx, spec.GetArtifactUrl(), f, 0, nil); err != nil {
		var status downloadStatusError
		if errors.As(err, &status) {
			return "", fmt.Errorf("downloading driver %s: HTTP %d", spec.GetName(), int(status))
		}
		return "", fmt.Errorf("downloading driver %s: %w", spec.GetName(), err)
	}
	// Hashed once complete: a resumed or restarted download is not one stream.
	got, err := sha256File(f.Name())
	if err != nil {
		return "", fmt.Errorf("downloading driver %s: %w", spec.GetName(), err)
	}
	if !strings.EqualFold(got, spec.GetSha256()) {
		return "", fmt.Errorf("driver %s: downloaded image SHA-256 does not match the manifest", spec.GetName())
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return f.Name(), nil
}
