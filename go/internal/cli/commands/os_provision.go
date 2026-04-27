package commands

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/wendylabsinc/wendy/internal/shared/certs"
	"github.com/wendylabsinc/wendy/internal/shared/config"
	"github.com/wendylabsinc/wendy/internal/shared/wendyconf"
	cloudpb "github.com/wendylabsinc/wendy/proto/gen/cloudpb"
)

// preProvisionedState is written to the config partition during imaging.
// JSON tags must match provisioningState in internal/agent/services.
type preProvisionedState struct {
	Enrolled  bool   `json:"enrolled"`
	CloudHost string `json:"cloudHost,omitempty"`
	OrgID     int32  `json:"orgId,omitempty"`
	AssetID   int32  `json:"assetId,omitempty"`
	KeyPEM    string `json:"keyPem,omitempty"`
	CertPEM   string `json:"certPem,omitempty"`
	ChainPEM  string `json:"chainPem,omitempty"`
}

// PreEnrollDialer creates a gRPC connection for pre-enrollment.
// Tests replace this with a dialer that connects to a local fake server.
type PreEnrollDialer func(ctx context.Context, addr string, opt grpc.DialOption) (*grpc.ClientConn, error)

func defaultPreEnrollDialer(_ context.Context, addr string, opt grpc.DialOption) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, opt)
}

// preEnrollDevice generates a device key pair, gets an enrollment token from
// Wendy Cloud, issues a certificate, and returns the provisioning state as JSON
// to be written to the config partition. deviceName is optional. Pass nil for
// dialer to use the default.
func preEnrollDevice(ctx context.Context, auth *config.AuthConfig, deviceName string, dialer PreEnrollDialer) ([]byte, error) {
	if dialer == nil {
		dialer = defaultPreEnrollDialer
	}

	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]

	var transportOpt grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		tlsCfg, err := certs.LoadTLSConfig(cert.PemCertificate, cert.PemCertificateChain, cert.PemPrivateKey, "")
		if err != nil {
			return nil, fmt.Errorf("loading TLS config: %w", err)
		}
		transportOpt = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transportOpt = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	cloudConn, err := dialer(ctx, auth.CloudGRPC, transportOpt)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	defer cloudConn.Close()

	certClient := cloudpb.NewCertificateServiceClient(cloudConn)

	tokenResp, err := certClient.CreateAssetEnrollmentToken(ctx, &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: int32(cert.OrganizationID),
		Name:           deviceName,
	})
	if err != nil {
		return nil, fmt.Errorf("creating enrollment token: %w", err)
	}
	orgID := tokenResp.GetOrganizationId()
	assetID := tokenResp.GetAssetId()

	// Generate key pair in memory only — never written to the local machine's disk.
	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generating key pair: %w", err)
	}

	csrPEM, err := certs.GenerateCSR(keyPEM, fmt.Sprintf("sh/wendy/%d/%d", orgID, assetID))
	if err != nil {
		return nil, fmt.Errorf("generating CSR: %w", err)
	}

	issueResp, err := certClient.IssueCertificate(ctx, &cloudpb.IssueCertificateRequest{
		PemCsr:          csrPEM,
		EnrollmentToken: tokenResp.GetEnrollmentToken(),
	})
	if err != nil {
		return nil, fmt.Errorf("issuing certificate: %w", err)
	}
	if issueResp.GetError() != nil {
		return nil, fmt.Errorf("certificate issuance failed: %s", issueResp.GetError().GetMessage())
	}
	certObj := issueResp.GetCertificate()
	if certObj == nil {
		return nil, fmt.Errorf("cloud returned empty certificate")
	}

	cloudHost := auth.CloudGRPC
	if h, _, splitErr := net.SplitHostPort(cloudHost); splitErr == nil {
		cloudHost = h
	}

	state := preProvisionedState{
		Enrolled:  true,
		CloudHost: cloudHost,
		OrgID:     orgID,
		AssetID:   assetID,
		KeyPEM:    keyPEM,
		CertPEM:   certObj.GetPemCertificate(),
		ChainPEM:  certObj.GetPemCertificateChain(),
	}
	return json.Marshal(state)
}

// writeConfigFiles writes the agent binary, optional wendy.conf, and optional
// provisioning.json to mountPoint.
func writeConfigFiles(mountPoint string, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	binPath := filepath.Join(mountPoint, "wendy-agent")
	if err := os.WriteFile(binPath, agentBinary, 0o755); err != nil {
		return fmt.Errorf("writing wendy-agent to config partition: %w", err)
	}

	if len(creds) > 0 || deviceName != "" {
		for _, c := range creds {
			if strings.ContainsAny(c.SSID, "\n\r") || strings.ContainsAny(c.Password, "\n\r") {
				return fmt.Errorf("WiFi SSID and password must not contain newline characters")
			}
		}
		if strings.ContainsAny(deviceName, "\n\r") {
			return fmt.Errorf("device name must not contain newline characters")
		}

		var conf []byte
		if len(creds) > 0 {
			conf = wendyconf.Marshal(creds)
		}
		if deviceName != "" {
			if len(conf) > 0 {
				conf = append(conf, '\n')
			}
			conf = append(conf, []byte(fmt.Sprintf("[device]\nname = %s\n", deviceName))...)
		}

		confPath := filepath.Join(mountPoint, "wendy.conf")
		if err := os.WriteFile(confPath, conf, 0o644); err != nil {
			return fmt.Errorf("writing wendy.conf to config partition: %w", err)
		}
	}

	if len(provisioningJSON) > 0 {
		provPath := filepath.Join(mountPoint, "provisioning.json")
		if err := os.WriteFile(provPath, provisioningJSON, 0o600); err != nil {
			return fmt.Errorf("writing provisioning.json to config partition: %w", err)
		}
	}

	return nil
}
