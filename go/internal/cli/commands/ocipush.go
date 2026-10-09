package commands

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/remote"
)

// ociLayoutManifestForPlatform opens dir as an OCI image layout and returns
// the v1.Image for the manifest pickOCIDescriptor would choose for platform —
// the newest manifest matching os/arch, skipping buildx's unknown/unknown
// attestation entries. Matching that selection exactly matters: it must be
// the same manifest deployByChunkDiff just read and diffed against the
// device (readOCILayoutDirLayers uses the identical pickOCIDescriptor call),
// or a registry-push fallback that reuses this layout could push a different
// image than the one the user just saw diffed.
func ociLayoutManifestForPlatform(dir, platform string) (v1.Image, error) {
	idx, err := layout.ImageIndexFromPath(dir)
	if err != nil {
		return nil, fmt.Errorf("opening OCI layout %s: %w", dir, err)
	}
	im, err := idx.IndexManifest()
	if err != nil {
		return nil, fmt.Errorf("reading OCI layout index: %w", err)
	}

	descs := make([]ociDescriptor, len(im.Manifests))
	for i, m := range im.Manifests {
		d := ociDescriptor{MediaType: string(m.MediaType), Digest: m.Digest.String()}
		if m.Platform != nil {
			d.Platform = &struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			}{Architecture: m.Platform.Architecture, OS: m.Platform.OS}
		}
		descs[i] = d
	}

	wantOS, wantArch := parseOCIPlatform(platform)
	chosen := pickOCIDescriptor(descs, wantOS, wantArch)
	if chosen == nil {
		return nil, fmt.Errorf("no image manifest for platform %s in OCI layout %s", platform, dir)
	}
	hash, err := v1.NewHash(chosen.Digest)
	if err != nil {
		return nil, fmt.Errorf("invalid manifest digest %q: %w", chosen.Digest, err)
	}
	img, err := idx.Image(hash)
	if err != nil {
		return nil, fmt.Errorf("reading image %s from OCI layout: %w", hash, err)
	}
	return img, nil
}

// registryPushTransport builds the http.RoundTripper pushOCILayoutToRegistry
// uses to reach the device registry at registryAddr, mirroring
// buildkitRegistryConfig's per-mode posture: plain HTTP for an
// unprovisioned device, or an mTLS handshake for a provisioned one.
//
// The mTLS case deliberately does NOT construct its own tls.Config — it
// reuses this package's existing tlsClientDialer (docker.go), the same
// helper resolveRegistryForSwiftAgent's cloud-tunnel path uses, so the
// device registry's certificate gets the exact same Wendy-CA chain
// validation (wendyRegistryServerVerifier) as every other mTLS path in this
// CLI, instead of a second, independently-reviewed TLS trust decision living
// here. insecureHTTP tells the caller to parse the registry reference with
// name.Insecure (plain http://, no TLS at all) rather than use this
// transport.
func registryPushTransport(registryAddr string, useMTLS bool) (transport http.RoundTripper, insecureHTTP bool, err error) {
	if !useMTLS {
		return http.DefaultTransport, true, nil
	}

	certInfo := loadCLICert()
	if certInfo == nil || certInfo.PemCertificate == "" || !certInfo.HasPrivateKey() {
		return nil, false, fmt.Errorf("mTLS connection but no CLI certificates available")
	}
	keyPEM, err := certInfo.PrivateKeyPEM()
	if err != nil {
		return nil, false, fmt.Errorf("loading client key: %w", err)
	}

	rawDial := func(dialCtx context.Context) (net.Conn, error) {
		return deviceLinkDialFn(dialCtx, registryAddr)
	}
	tlsDial, err := tlsClientDialer(certInfo.PemCertificate, keyPEM, certInfo.PemCertificateChain, rawDial)
	if err != nil {
		return nil, false, fmt.Errorf("preparing mTLS dialer for device registry: %w", err)
	}

	return &http.Transport{
		DialTLSContext: func(dialCtx context.Context, _, _ string) (net.Conn, error) {
			return tlsDial(dialCtx)
		},
	}, false, nil
}

// pushOCILayoutToRegistry pushes the manifest ociLayoutManifestForPlatform
// selects straight to registryAddr/repo:latest using go-containerregistry,
// without invoking docker or buildx. It exists so the registry-push fallback
// can reuse an OCI-layout image the chunk-diff deploy already built instead
// of re-running an entire buildx build a second time: buildx itself can't
// share the result between the two call sites because they use different
// output types ("type=oci" for the layout export vs "type=image,push=true"
// for a registry push), but the underlying image content is identical given
// the same Dockerfile, build-args, and platform — so a raw registry push of
// the already-built manifest is equivalent to rebuilding and pushing it,
// without paying for the rebuild.
func pushOCILayoutToRegistry(ctx context.Context, layoutDir, platform, registryAddr, repo string, useMTLS bool) error {
	img, err := ociLayoutManifestForPlatform(layoutDir, platform)
	if err != nil {
		return err
	}

	transport, insecureHTTP, err := registryPushTransport(registryAddr, useMTLS)
	if err != nil {
		return err
	}

	var nameOpts []name.Option
	if insecureHTTP {
		nameOpts = append(nameOpts, name.Insecure)
	}
	ref, err := name.ParseReference(fmt.Sprintf("%s/%s:latest", registryAddr, repo), nameOpts...)
	if err != nil {
		return fmt.Errorf("parsing registry reference: %w", err)
	}

	// Retry transient registry/push failures for the same reason
	// buildAndPushImage does (WDY-1690): images push to the device registry
	// through one shared tunnel that can briefly collapse under concurrent
	// load, surfacing as timeouts/connection resets rather than a genuine
	// push failure.
	var lastErr error
	for attempt := 1; attempt <= maxBuildPushAttempts; attempt++ {
		lastErr = remote.Write(ref, img,
			remote.WithContext(ctx),
			remote.WithTransport(transport),
			remote.WithAuth(authn.Anonymous),
		)
		if lastErr == nil {
			return nil
		}
		if !shouldRetryPush(ctx.Err(), attempt, lastErr.Error(), nil) {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("pushing OCI layout to registry: %w", lastErr)
		case <-time.After(buildPushRetryBackoff(attempt)):
		}
	}
	return fmt.Errorf("pushing OCI layout to registry: %w", lastErr)
}
