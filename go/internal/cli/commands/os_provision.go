package commands

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/enrolltoken"
	"github.com/wendylabsinc/wendy/go/internal/shared/wendyconf"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// PreProvisionedState is written to the config partition during imaging.
// JSON tags must match provisioningState in internal/agent/services.
type PreProvisionedState struct {
	Enrolled  bool   `json:"enrolled"`
	CloudHost string `json:"cloudHost,omitempty"`
	OrgID     int32  `json:"orgId,omitempty"`
	AssetID   int32  `json:"assetId,omitempty"`
	KeyPEM    string `json:"keyPem,omitempty"`
	CertPEM   string `json:"certPem,omitempty"`
	ChainPEM  string `json:"chainPem,omitempty"`
}

type PreEnrollDialer func(ctx context.Context, addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error)

func defaultPreEnrollDialer(_ context.Context, addr string, opts ...grpc.DialOption) (*grpc.ClientConn, error) {
	return grpc.NewClient(addr, opts...)
}

// preEnrollDevice generates a device key pair, gets an enrollment token from
// Wendy Cloud, issues a certificate, and returns the provisioning state.
// deviceName is optional. orgID selects the target organization; pass 0 to
// fall back to the org embedded in the auth certificate. Pass nil for dialer
// to use the default.
func preEnrollDevice(ctx context.Context, auth *config.AuthConfig, deviceName string, orgID int32, dialer PreEnrollDialer) (*PreProvisionedState, error) {
	if dialer == nil {
		dialer = defaultPreEnrollDialer
	}

	if len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("auth entry has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]

	var transportOpt grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		keyPEM, err := cert.PrivateKeyPEM()
		if err != nil {
			return nil, fmt.Errorf("loading client key: %w", err)
		}
		tlsCfg, err := certs.LoadTLSConfig(cert.PemCertificate, cert.PemCertificateChain, keyPEM, "")
		if err != nil {
			return nil, fmt.Errorf("loading TLS config: %w", err)
		}
		transportOpt = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transportOpt = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	dialOptions, err := withCloudRequestSigning(auth, transportOpt)
	if err != nil {
		return nil, err
	}
	cloudConn, err := dialer(ctx, auth.CloudGRPC, dialOptions...)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	defer cloudConn.Close()

	certClient := cloudpb.NewCertificateServiceClient(cloudConn)

	tokenCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return nil, err
	}

	if orgID == 0 {
		orgID = int32(cert.OrganizationID)
	}
	tokenResp, err := certClient.CreateAssetEnrollmentToken(tokenCtx, &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: orgID,
		Name:           deviceName,
	})
	if err != nil {
		return nil, fmt.Errorf("creating enrollment token: %w", err)
	}
	resolvedOrgID := tokenResp.GetOrganizationId()
	assetID := tokenResp.GetAssetId()

	// Generate key pair in memory only — never written to the local machine's disk.
	keyPEM, err := certs.GenerateKeyPair()
	if err != nil {
		return nil, fmt.Errorf("generating key pair: %w", err)
	}

	// Device identity acts as both a TLS client (to the cloud) and a TLS server
	// (agent gRPC and tunnel endpoints), so request both EKUs.
	identityURIs := []string{certs.AssetURN(resolvedOrgID, assetID)}
	if spiffeURI, ok := enrolltoken.TenantSPIFFEURIFromToken(tokenResp.GetEnrollmentToken()); ok {
		identityURIs = append(identityURIs, spiffeURI)
	}
	csrPEM, err := certs.GenerateCSR([]byte(keyPEM), fmt.Sprintf("sh/wendy/%d/%d", resolvedOrgID, assetID),
		identityURIs,
		x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth)
	if err != nil {
		return nil, fmt.Errorf("generating CSR: %w", err)
	}

	issueResp, err := certClient.IssueCertificate(tokenCtx, &cloudpb.IssueCertificateRequest{
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

	state := &PreProvisionedState{
		Enrolled:  true,
		CloudHost: auth.CloudGRPC,
		OrgID:     resolvedOrgID,
		AssetID:   assetID,
		KeyPEM:    keyPEM,
		CertPEM:   certObj.GetPemCertificate(),
		ChainPEM:  certObj.GetPemCertificateChain(),
	}
	return state, nil
}

// psPartition is one row from the Windows partition-listing PowerShell
// script. Pointer fields tolerate JSON nulls for partitions without a
// drive letter or associated volume (e.g. EFI or reserved partitions).
type psPartition struct {
	PartitionNumber int     `json:"PartitionNumber"`
	DriveLetter     *string `json:"DriveLetter"`
	Label           *string `json:"Label"`
	Size            int64   `json:"Size"`
}

func parseConfigPartition(jsonBytes []byte) (int, error) {
	trimmed := strings.TrimSpace(string(jsonBytes))
	if trimmed == "" {
		return 0, fmt.Errorf("no partitions found on disk (is the image fully written?)")
	}

	var parts []psPartition
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &parts); err != nil {
			return 0, fmt.Errorf("parsing partition JSON: %w", err)
		}
	} else {
		var single psPartition
		if err := json.Unmarshal([]byte(trimmed), &single); err != nil {
			return 0, fmt.Errorf("parsing partition JSON: %w", err)
		}
		parts = []psPartition{single}
	}

	var matches []int
	for _, p := range parts {
		if p.Label == nil {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(*p.Label), "config") {
			matches = append(matches, p.PartitionNumber)
		}
	}
	switch len(matches) {
	case 0:
		return 0, fmt.Errorf("no partition labelled 'config' found on disk (is the image fully written?)")
	case 1:
		return matches[0], nil
	default:
		return 0, fmt.Errorf("multiple partitions labelled 'config' found (%v); refusing to guess which to mount", matches)
	}
}

// provisioningRequired reports whether the user supplied provisioning data
// that must reach the device's config partition. When this returns true, a
// failure to write the config partition has dropped user-visible state on
// the floor and must be treated as fatal — silently printing "successfully
// installed" hides the lost data (--wifi never reaches the device, the
// pre-enroll key/cert is discarded, etc.).
func provisioningRequired(creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) bool {
	return len(creds) > 0 || deviceName != "" || len(provisioningJSON) > 0
}

// configTarget receives the config-partition files: a mounted directory (disk
// installs) or a FAT32 image (Thor).
type configTarget interface {
	WriteFile(name string, data []byte, perm os.FileMode) error
}

// dirTarget writes into a mounted config-partition directory.
type dirTarget string

func (d dirTarget) WriteFile(name string, data []byte, perm os.FileMode) error {
	return os.WriteFile(filepath.Join(string(d), name), data, perm)
}

func writeConfigFiles(mountPoint string, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	return writeConfigFilesTo(dirTarget(mountPoint), agentBinary, creds, deviceName, provisioningJSON)
}

// writeConfigFilesTo writes the config-partition files to any target. agentBinary
// is skipped when empty (Thor omits it only if a caller passes none).
func writeConfigFilesTo(t configTarget, agentBinary []byte, creds []wendyconf.WifiCredential, deviceName string, provisioningJSON []byte) error {
	if len(agentBinary) > 0 {
		if err := t.WriteFile("wendy-agent", agentBinary, 0o755); err != nil {
			return fmt.Errorf("writing wendy-agent to config partition: %w", err)
		}
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

		if err := t.WriteFile("wendy.conf", conf, 0o644); err != nil {
			return fmt.Errorf("writing wendy.conf to config partition: %w", err)
		}
	}

	if len(provisioningJSON) > 0 {
		if err := t.WriteFile("provisioning.json", provisioningJSON, 0o600); err != nil {
			return fmt.Errorf("writing provisioning.json to config partition: %w", err)
		}
	}

	// Clock floor so the device boots with a sane clock even before NTP/Roughtime sync.
	if err := t.WriteFile("clock_floor", timesync.FloorBytes(time.Now()), 0o644); err != nil {
		return fmt.Errorf("writing clock_floor to config partition: %w", err)
	}

	return nil
}

func buildWendyConf(creds []wendyconf.WifiCredential, deviceName string, state *PreProvisionedState) (*litepb.WendyConf, error) {
	conf := &litepb.WendyConf{}

	if deviceName != "" {
		conf.DeviceName = &deviceName
	}

	if len(creds) > 0 {
		networks := make([]*litepb.WendyConfWifiNetwork, len(creds))
		for i, c := range creds {
			networks[i] = &litepb.WendyConfWifiNetwork{
				Ssid:     c.SSID,
				Password: c.Password,
				Priority: c.Priority,
				Hidden:   c.Hidden,
				Security: wifiSecurityToProto(c.Security),
			}
		}
		conf.Wifi = &litepb.WendyConfWifi{Networks: networks}
	}

	if state != nil {
		keyDER, err := pemBlockToDER(state.KeyPEM)
		if err != nil {
			return nil, fmt.Errorf("converting key PEM to DER: %w", err)
		}
		certDER, err := pemBlockToDER(state.CertPEM)
		if err != nil {
			return nil, fmt.Errorf("converting cert PEM to DER: %w", err)
		}
		chainDER, err := pemChainToDER(state.ChainPEM)
		if err != nil {
			return nil, fmt.Errorf("converting chain PEM to DER: %w", err)
		}
		conf.Provisioning = &litepb.WendyConfCloudProvisioning{
			Enrolled:  state.Enrolled,
			CloudHost: state.CloudHost,
			OrgId:     state.OrgID,
			AssetId:   state.AssetID,
			Key:       keyDER,
			Cert:      certDER,
			Chain:     chainDER,
		}
	}

	return conf, nil
}

// pemBlockToDER decodes the first PEM block from s and returns its raw DER bytes.
func pemBlockToDER(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, fmt.Errorf("invalid PEM data")
	}
	return block.Bytes, nil
}

// pemChainToDER decodes all PEM blocks from s and concatenates their DER bytes.
func pemChainToDER(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	var der []byte
	rest := []byte(s)
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		der = append(der, block.Bytes...)
	}
	if len(der) == 0 {
		return nil, fmt.Errorf("invalid PEM chain data")
	}
	return der, nil
}

func wifiSecurityToProto(s string) litepb.WendyConfWifiSecurity {
	switch s {
	case "open":
		return litepb.WendyConfWifiSecurity_WENDY_CONF_WIFI_SECURITY_OPEN
	case "wpa2":
		return litepb.WendyConfWifiSecurity_WENDY_CONF_WIFI_SECURITY_WPA2
	case "wpa3":
		return litepb.WendyConfWifiSecurity_WENDY_CONF_WIFI_SECURITY_WPA3
	default:
		return litepb.WendyConfWifiSecurity_WENDY_CONF_WIFI_SECURITY_UNSPECIFIED
	}
}
