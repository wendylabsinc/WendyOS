package commands

import (
	"context"
	"fmt"
	"os"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

type osUpdateArtifactOptions struct {
	url, path, targetVersion, driversDir string
	pr                                   int
	noDrivers                            bool
}

// prepareOSUpdateArtifact runs the existing safety checks before downloading or
// serving the OS image. Cleanup owns the temporary server and a downloaded image,
// never a caller-supplied file. The caller keeps both alive through the update.
func prepareOSUpdateArtifact(ctx context.Context, conn *grpcclient.AgentConnection, versionResp *agentpb.GetAgentVersionResponse, opts osUpdateArtifactOptions) (url string, cleanup func(), err error) {
	url = opts.url
	cleanup = func() {}
	defer func() {
		if err != nil {
			cleanup()
		}
	}()

	localPath := ""
	if opts.path != "" {
		localPath, err = resolveArtifactPath(opts.path)
		if err != nil {
			return "", cleanup, err
		}
		url = localPath // Validate the actual artifact before starting its server.
	}
	if url == "" {
		return "", cleanup, fmt.Errorf("provide a local artifact path or --artifact-url")
	}
	if err := osUpdateStackMismatch(versionResp, url); err != nil {
		return "", cleanup, err
	}
	if opts.driversDir != "" {
		if err := checkDriversDir(conn, opts.driversDir); err != nil {
			return "", cleanup, err
		}
	}
	pf := warnDriverAddonsBeforeUpdate(ctx, conn, versionResp.GetDeviceType(), opts.targetVersion, opts.driversDir, opts.pr, !opts.noDrivers)
	if !opts.noDrivers && pf.blocking() {
		if err := confirmDriverPreflight(pf); err != nil {
			return "", cleanup, err
		}
	}

	// Loopback reaches a port-forwarded device, which can download through NAT.
	if localPath == "" && !isLoopbackHost(conn.Host) && !deviceHasWiFi(ctx, conn) {
		fmt.Println("Device is not connected to WiFi — downloading artifact to serve locally...")
		localPath, err = downloadArtifactToTemp(url)
		if err != nil {
			return "", cleanup, fmt.Errorf("downloading artifact: %w", err)
		}
		cleanup = func() { os.Remove(localPath) }
	}
	if localPath == "" {
		return url, cleanup, nil
	}
	localIP, err := localIPForHost(ctx, conn.Host)
	if err != nil {
		return "", cleanup, fmt.Errorf("determining local IP for device %s: %w", conn.Host, err)
	}
	servedURL, stopServer, err := serveLocalArtifact(localPath, localIP)
	if err != nil {
		return "", cleanup, err
	}
	removeDownload := cleanup
	cleanup = func() {
		stopServer()
		removeDownload()
	}
	fmt.Printf("Serving artifact at: %s\n", servedURL)
	return servedURL, cleanup, nil
}
