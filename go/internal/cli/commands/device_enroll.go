package commands

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// acmeEndpointEnv names pki-core's ACME frontend, e.g.
// "https://acme.pki.example". The tenant path and "/acme/directory" are
// appended; only the origin is configuration.
//
// As with the renew frontend, there is deliberately NO derivation from the
// cloud endpoint. Guessing one service's host from another's is what sent
// enrollment tokens to the wrong place in cleartext (WDY-2799).
const acmeEndpointEnv = "WENDY_PKI_ACME_ENDPOINT"

// acmeDirectoryURL builds the tenant's ACME directory from the configured
// frontend origin. The tenant is a canonical lower-case UUID and rides inside
// the URL, which is why acmeenroll.Config carries no tenant field.
func acmeDirectoryURL(tenant string) (string, error) {
	endpoint := strings.TrimSpace(os.Getenv(acmeEndpointEnv))
	if endpoint == "" {
		return "", fmt.Errorf("no pki-core ACME frontend configured; set %s (e.g. https://acme.pki.example)", acmeEndpointEnv)
	}
	base, err := url.Parse(endpoint)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return "", fmt.Errorf("%s is not a valid URL: %q", acmeEndpointEnv, endpoint)
	}
	base.Path = strings.TrimSuffix(base.Path, "/") + "/" + tenant + "/acme/directory"
	return base.String(), nil
}

// runEnrollDevice obtains a single-use enrollment credential for the connected
// device and stages it on the agent, which redeems it itself against pki-core.
//
// No certificate and no private key passes through this command. The operator's
// signature on the enrollment request is the authority pki-core verifies; cloud
// relays it and can withhold the request, but cannot issue anything.
func runEnrollDevice(ctx context.Context, conn *grpcclient.AgentConnection, auth *config.AuthConfig, name string, orgOverride int32) error {
	if auth == nil || len(auth.Certificates) == 0 {
		return fmt.Errorf("selected auth entry has no certificates; re-run 'wendy auth login'")
	}
	if orgOverride != 0 {
		// Kept working rather than removed: the organization now comes from the
		// operator certificate's tenant, so there is nothing left for the flag
		// to select. Saying so is better than honouring it silently.
		fmt.Fprintf(os.Stderr, "warning: --org is ignored; the device is enrolled into the organization your operator certificate is bound to\n")
	}

	name, err := resolveEnrollmentName(conn.Host, name)
	if err != nil {
		return err
	}
	// The device id is a v4 UUID, and is deliberately unrelated to the name.
	// The two are different kinds of key: the name is unique within the
	// organization and is what devices are addressed and discovered by, so it
	// stays changeable with `wendy device rename`; this id is globally unique,
	// is fixed at mint and is carried by every certificate the device is ever
	// issued, so tying it to a name that can be reassigned would be a mistake.
	deviceID := uuid.NewString()

	signer, err := cloudrequest.NewEnrollmentSigner(auth)
	if err != nil {
		return err
	}
	directoryURL, err := acmeDirectoryURL(signer.Tenant())
	if err != nil {
		return err
	}

	// Class B: an EAB with no attestation challenge, which is the only class
	// the agent can redeem today. Class A needs a TPM attestation the ACME
	// client does not implement, and class C needs an EST client that does not
	// exist yet — asking for either would burn a single-use credential the
	// device could not spend.
	enrollmentJWS, err := signer.SignEnrollmentRequest(deviceID, cloudrequest.DeviceClassB)
	if err != nil {
		return err
	}

	cloudConn, err := dialCloudForEnrollment(auth)
	if err != nil {
		return err
	}
	defer cloudConn.Close()

	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}

	fmt.Printf("Enrolling %s as %s...\n", name, deviceID)
	resp, err := cloudpbv2.NewDeviceEnrollmentServiceClient(cloudConn).EnrollDevice(cloudCtx, &cloudpbv2.EnrollDeviceRequest{
		DeviceId:    deviceID,
		DeviceClass: cloudpbv2.DeviceClass_DEVICE_CLASS_B,
		// Relayed byte-identically all the way to pki-core: re-serializing it
		// anywhere along the way would invalidate the operator's signature.
		EnrollmentRequestJws: enrollmentJWS,
		Name:                 name,
	})
	if err != nil {
		// Cloud validates the name and checks for a collision BEFORE relaying
		// to pki-core, so these two refusals cost nothing and are worth naming:
		// the operator can retry immediately with a different name.
		switch status.Code(err) {
		case codes.AlreadyExists:
			return fmt.Errorf("the name %q is already taken by another device in your organization "+
				"(names are compared without regard to case). Nothing was minted — re-run with a different --name: %w", name, err)
		case codes.InvalidArgument:
			return fmt.Errorf("cloud refused the enrollment request: %w "+
				"(nothing was minted)", err)
		}
		return fmt.Errorf("requesting enrollment credential: %w", err)
	}

	// From here the credential is spent whatever happens: there is no retrieval
	// RPC and cloud never stores the secret. Every failure below has to say so,
	// because the remedy is always a fresh enrollment rather than a retry.
	if kind := resp.GetCredentialKind(); kind != "eab" {
		return fmt.Errorf(
			"cloud returned a %q credential, which this CLI cannot stage (only ACME external account bindings are supported; EST is not built yet). "+
				"That credential is single-use and is now spent", kind)
	}

	_, err = conn.ProvisioningService.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{
		Acme: &agentpb.AcmeEnrollment{
			DirectoryUrl: directoryURL,
			DeviceId:     deviceID,
			EabKeyId:     resp.GetEabKeyId(),
			// Hex, and passed through as such: the agent decodes it.
			EabHmacKey: resp.GetEabHmacKey(),
		},
	})
	if err != nil {
		return fmt.Errorf("staging the enrollment credential on the device: %w "+
			"(the credential is single-use and is now spent; run 'wendy device enroll' again for a fresh one)", err)
	}

	fmt.Printf("Device enrolled (name: %s, identity: %s, asset: %s).\n", name, deviceID, resp.GetAssetId())
	return nil
}

// dialCloudForEnrollment builds the cloud connection, with the operator
// certificate presented for mTLS and the request-signature interceptor
// installed — EnrollDevice is an authorization-gated mutation and cloud refuses
// it outright without a signed request descriptor.
func dialCloudForEnrollment(auth *config.AuthConfig) (*grpc.ClientConn, error) {
	cert := auth.Certificates[0]
	transport := grpc.WithTransportCredentials(insecure.NewCredentials())
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		keyPEM, err := cert.PrivateKeyPEM()
		if err != nil {
			return nil, fmt.Errorf("loading client key: %w", err)
		}
		tlsCfg, err := certs.LoadTLSConfig(cert.PemCertificate, cert.PemCertificateChain, keyPEM, "")
		if err != nil {
			return nil, fmt.Errorf("loading TLS config: %w", err)
		}
		transport = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	}
	dialOptions, err := withCloudRequestSigning(auth, transport)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.NewClient(auth.CloudGRPC, dialOptions...)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	return conn, nil
}
