package browserauth

import (
	"context"
	"crypto"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudunenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/litetopology"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// ConnectDevice keeps tunnel authorization, grant verification and device mTLS
// in the caller. dial carries public Cloud/broker HTTP/2 over verified TLS.
func (s *Session) ConnectDevice(ctx context.Context, asset string, dial func(context.Context, string) (net.Conn, error), client *http.Client) (*grpcclient.AgentConnection, error) {
	conn, _, _, err := s.connectReporter(ctx, asset, dial, client, false)
	return conn, err
}

func (s *Session) connectReporter(ctx context.Context, asset string, dial func(context.Context, string) (net.Conn, error), httpClient *http.Client, topology bool) (*grpcclient.AgentConnection, *liteReporter, context.Context, error) {
	lifetime := ctx
	cancelNormal := func() {}
	retained := false
	if topology {
		ctx, cancelNormal = context.WithTimeout(ctx, browserTopologyTimeout)
	}
	defer func() {
		if !retained {
			cancelNormal()
		}
	}()
	if topology {
		if err := s.lockReporterIdentity(ctx); err != nil {
			return nil, nil, nil, err
		}
	} else {
		s.mu.Lock()
	}
	locked := true
	defer func() {
		if locked {
			s.mu.Unlock()
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	if !canonicalUUID(asset) {
		return nil, nil, nil, fmt.Errorf("Invalid cloud device ID")
	}
	if s.tokens.Access == "" {
		return nil, nil, nil, fmt.Errorf("Sign in with Wendy first")
	}
	if err := s.refreshCloudToken(ctx); err != nil {
		return nil, nil, nil, err
	}
	cert, access, key := s.certificate, s.tokens.Access, s.key
	settings, tenant, privatePEM := s.settings(), s.profile.Tenant, s.privatePEM
	auth := &config.AuthConfig{CloudGRPC: settings.CloudGRPC, OAuthIssuer: s.meta.Issuer, DPoPPrivateKey: privatePEM, Certificates: []config.CertificateInfo{cert}}
	// Token refresh and the identity snapshot are serialized. Device tunnels
	// are independent: a slow peer must not block every topology observation.
	locked = false
	s.mu.Unlock()
	opts := []grpc.DialOption{clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) { return dial(c, settings.CloudGRPC) }), grpc.WithTransportCredentials(insecure.NewCredentials())}
	opts = append(opts, cloudrequest.DPoPDialOptions(auth, func(context.Context) (string, crypto.Signer, error) { return access, key, nil })...)
	cloud, err := grpc.NewClient("passthrough:///"+settings.CloudGRPC, opts...)
	if err != nil {
		return nil, nil, nil, err
	}
	signer, err := cloudrelay.PrincipalSigner(cert.PemCertificate, []byte(privatePEM))
	if err != nil {
		cloud.Close()
		return nil, nil, nil, err
	}
	verifier := &cloudrelay.Verifier{Issuer: settings.RelayIssuer, HTTP: httpClient, RelayDial: func(endpoint string) (*grpc.ClientConn, error) {
		target, err := cloudrelay.BrowserBrokerTarget(endpoint)
		if err != nil {
			return nil, err
		}
		return grpc.NewClient("passthrough:///"+target, grpc.WithTransportCredentials(insecure.NewCredentials()), clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) { return dial(c, target) }))
	}}
	record, err := cloudpbv2.NewAssetServiceClient(cloud).GetAsset(ctx, &cloudpbv2.GetAssetRequest{Id: asset})
	if err != nil {
		cloud.Close()
		return nil, nil, nil, fmt.Errorf("Looking up device certificate identity: %w", err)
	}
	expected, err := cloudDeviceIdentity(record, asset, tenant)
	if err != nil {
		cloud.Close()
		return nil, nil, nil, err
	}

	if topology && liteAsset(record) {
		cancelNormal()
		setup, finishSetup, cancelSetup := litetopology.BeginSetup(lifetime, browserLiteSetupBudget)
		reporter, err := s.connectLiteReporter(setup, cloud, asset, expected, dial, httpClient)
		if err == nil {
			err = finishSetup()
		}
		if err != nil {
			cancelSetup()
		} else {
			reporter.cancel = cancelSetup
		}
		if err != nil {
			cloud.Close()
			return nil, nil, nil, err
		}
		return nil, reporter, lifetime, nil
	}

	conn, err := grpcclient.ConnectWithTLSExpecting(ctx, "passthrough:///cloud-device", &cert, nil, expected, clouddefaults.TunnelDialer(func(c context.Context) (net.Conn, error) {
		return cloudrelay.OpenTCP(c, c, cloud, verifier, asset, "wendy-agent", signer)
	}))
	if err != nil {
		cloud.Close()
		return nil, nil, nil, err
	}
	conn.ExtraClosers = append(conn.ExtraClosers, cloud, cancelReporter(cancelNormal))
	retained = true
	return conn, nil, ctx, nil
}

// Cloud asset IDs identify inventory rows. Only the enrollment binding names
// the certificate principal. Never learn this binding from the peer itself.
func cloudDeviceIdentity(asset *cloudpbv2.Asset, id, tenant string) (*certs.WendyIdentity, error) {
	if asset.GetId() != id || asset.GetOrganizationId() != tenant {
		return nil, fmt.Errorf("Cloud returned a device belonging to a different asset or tenant")
	}
	name := cloudunenroll.DeviceID(asset)
	if name == "" {
		return nil, fmt.Errorf("Cloud has no certificate identity recorded for this device")
	}
	for _, segment := range strings.Split(name, "/") {
		if len(segment) == 0 || len(segment) > 64 || segment == "." || segment == ".." {
			return nil, fmt.Errorf("Cloud returned an invalid device certificate identity")
		}
		for _, c := range segment {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
				return nil, fmt.Errorf("Cloud returned an invalid device certificate identity")
			}
		}
	}
	identity, err := certs.ParsePrincipal("spiffe://wendy.sh/tenant/" + tenant + "/device/" + name)
	if err != nil {
		return nil, err
	}
	return &identity, nil
}

type cancelReporter context.CancelFunc

func (c cancelReporter) Close() error { c(); return nil }

// Cold Lite certificate issuance serializes token rotation under mu. Reporter
// admission has its own mutex, and waiting for identity respects this reporter
// deadline rather than holding every other observation behind that operation.
func (s *Session) lockReporterIdentity(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.mu.TryLock() {
		return nil
	}
	retry := time.NewTicker(5 * time.Millisecond)
	defer retry.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-retry.C:
			if s.mu.TryLock() {
				return nil
			}
		}
	}
}
