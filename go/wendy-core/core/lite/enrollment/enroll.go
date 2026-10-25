// Package liteenroll prepares operator-authorized Tier C enrollment.
// Firmware creates its own key and redeems the credential directly at PKI.
package liteenroll

import (
	"context"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/cloud"
	cloudpb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/enrollment/internal/cloudpb"
	litepb "github.com/wendylabsinc/WendyOS/go/wendy-core/core/lite/pb"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"google.golang.org/protobuf/proto"
)

// Session contains public selected-session data; keys remain in the signer.
type Session struct {
	PKIEndpoint, CloudGRPC, PrincipalURI string
	HasCertificates                      bool
}

// Config resolves PKI endpoints and the Lite relay hostname from the selected
// session. The Lite relay has separate TCP ingress from the WendyOS listener.
func Config(auth *Session, deviceID, csrURL, timeURL, brokerHost string, brokerPort uint32) (*litepb.WendyConfEnrollment, error) {
	if auth == nil || !auth.HasCertificates {
		return nil, fmt.Errorf("enrollment requires a PKI operator session; run 'wendy cloud login'")
	}
	if brokerHost == "" {
		var err error
		brokerHost, err = liteBrokerHost(auth.CloudGRPC)
		if err != nil {
			return nil, fmt.Errorf("derive WendyCom broker hostname; pass --broker-host: %w", err)
		}
	}
	tenant, err := operatorTenant(auth.PrincipalURI)
	if err != nil {
		return nil, fmt.Errorf("enrollment requires a tenant operator identity")
	}
	if !canonicalUUID(deviceID) {
		return nil, fmt.Errorf("device ID must be a canonical lowercase UUID")
	}
	if csrURL == "" {
		host := PKISiblingHost(auth.PKIEndpoint, "csr")
		if host == "" {
			return nil, fmt.Errorf("pass --csr-url for this PKI deployment")
		}
		csrURL = "https://" + host + "/v1/" + tenant
	}
	if timeURL == "" {
		timeURL = DefaultTimeURL
	}
	if timeURL == "roughtime" {
		return nil, fmt.Errorf("mbedTLS Lite firmware uses signed RFC 3161 time; omit --time-url to use Sigstore")
	}
	endpoints := []string{csrURL}
	if timeURL != "roughtime" {
		endpoints = append(endpoints, timeURL)
	}
	for _, endpoint := range endpoints {
		u, err := url.Parse(endpoint)
		if err != nil || len(endpoint) > 256 || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" {
			return nil, fmt.Errorf("enrollment endpoints must be HTTPS URLs without credentials, query or fragment")
		}
	}
	u, _ := url.Parse(csrURL)
	if u.Path != "/v1/"+tenant {
		return nil, fmt.Errorf("CSR URL must end in /v1/%s for the selected tenant", tenant)
	}
	if len(brokerHost) == 0 || len(brokerHost) > 253 || strings.ContainsAny(brokerHost, "/\\@ \t\r\n") || brokerPort == 0 || brokerPort > 65535 {
		return nil, fmt.Errorf("pass a broker hostname and port between 1 and 65535")
	}
	return &litepb.WendyConfEnrollment{TenantId: tenant, DeviceId: deviceID, CsrUrl: csrURL, TimeUrl: timeURL, BrokerHost: brokerHost, BrokerPort: brokerPort}, nil
}

// Mint obtains the single-use credential after Cloud reserves the device asset.
// A device UUID Cloud refuses as taken is replaced through the shared
// collision guard; cfg.DeviceId ends as the UUID Cloud accepted.
func Mint(ctx context.Context, transport platform.Transport, signer *cloud.Signer, entropy platform.Entropy, cfg *litepb.WendyConfEnrollment, name string, hardware *DeviceHardware, profile TrustProfile) (asset string, err error) {
	if !cfg.ProvisionTrust {
		// Firmware-pinned roots cannot trust a newly selected authority until
		// its roots have been provisioned over USB.
		profile = profile.WithExistingDeviceIssuer()
	}
	if hardware == nil {
		hardware = &cloudpb.DeviceHardware{}
	} else {
		hardware = proto.Clone(hardware).(*cloudpb.DeviceHardware)
	}
	// Cloud requires this exact type before accepting Lite broker presence.
	// Class C controls credential issuance but does not set the asset's OS type.
	hardware.Os = proto.String("wendy-lite")
	err = RetryTakenDeviceUUID(ctx, entropy, cfg.DeviceId, func(deviceID string) error {
		cfg.DeviceId = deviceID
		var err error
		asset, err = mintOnce(ctx, transport, signer, cfg, name, hardware, profile)
		return err
	})
	return asset, err
}

func mintOnce(ctx context.Context, transport platform.Transport, signer *cloud.Signer, cfg *litepb.WendyConfEnrollment, name string, hardware *DeviceHardware, profile TrustProfile) (string, error) {
	artifact, err := signer.EnrollmentRequest(ctx, cfg.DeviceId, "C", profile.chainAlgorithm())
	if err != nil {
		return "", err
	}
	reply := new(cloudpb.EnrollDeviceResponse)
	response, err := signer.Invoke(ctx, transport, "/wendycloud.v2.DeviceEnrollmentService/EnrollDevice", &cloudpb.EnrollDeviceRequest{DeviceId: cfg.DeviceId, DeviceClass: cloudpb.DeviceClass_DEVICE_CLASS_C, EnrollmentRequestJws: artifact, Name: name, Hardware: hardware}, cloud.InvokeOptions{})
	if DeviceUUIDTaken(err, response.TrailingMetadata) {
		return "", fmt.Errorf("Cloud refused device UUID %s before minting anything: %w", cfg.DeviceId, ErrDeviceUUIDTaken)
	}

	if err != nil {
		return "", fmt.Errorf("creating Tier C enrollment: %w", err)
	}
	if err := proto.Unmarshal(response.Message, reply); err != nil {
		return "", err
	}
	// Cloud adopts the minted UUID as the asset id. Any other id would leave
	// the board's certificate naming a device Cloud does not know.
	if reply.GetAssetId() != cfg.DeviceId {
		return reply.GetAssetId(), fmt.Errorf("Cloud registered asset %q instead of the minted device UUID %s", reply.GetAssetId(), cfg.DeviceId)
	}
	expires, err := time.Parse(time.RFC3339, reply.GetExpiresAt())
	if reply.GetCredentialKind() != "enrollment_token" || len(reply.GetTokenValue()) == 0 || len(reply.GetTokenValue()) > 512 || strings.ContainsAny(reply.GetTokenValue(), "\r\n") || err != nil || time.Until(expires) < 30*time.Second {
		return reply.GetAssetId(), fmt.Errorf("Cloud returned an invalid or nearly expired Tier C enrollment credential")
	}
	cfg.Token = reply.GetTokenValue()
	return reply.GetAssetId(), nil
}

// SignedTime obtains a device-nonce-bound timestamp over verified HTTPS. The
// device verifies the CMS signature and pinned TSA chain before using the time.
func SignedTime(ctx context.Context, client platform.LiteHTTPTransport, endpoint, nonceHex string) ([]byte, error) {
	nonce, err := hex.DecodeString(nonceHex)
	if err != nil || len(nonce) != 32 {
		return nil, fmt.Errorf("device returned an invalid time nonce")
	}
	sum := sha256.Sum256(nonce)
	request := struct {
		Version int
		Imprint struct {
			Algorithm pkix.AlgorithmIdentifier
			Digest    []byte
		}
		Nonce   *big.Int
		CertReq bool
	}{Version: 1, Nonce: new(big.Int).SetBytes(nonce), CertReq: true}
	request.Imprint.Algorithm.Algorithm = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	request.Imprint.Digest = sum[:]
	der, err := asn1.Marshal(request)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "https" {
		return nil, fmt.Errorf("signed time requires HTTPS")
	}
	resp, err := client.DoLite(ctx, platform.HTTPRequest{Method: "POST", URL: endpoint, Headers: platform.HTTPHeaders{"Content-Type": {"application/timestamp-query"}}, Body: der})

	if err != nil {
		return nil, fmt.Errorf("getting signed PKI time: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("PKI time endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > 65536 {
		return nil, fmt.Errorf("PKI time response has invalid size")
	}
	return body, nil
}

// DeviceHardware is the existing Cloud enrollment hardware report.
type DeviceHardware = cloudpb.DeviceHardware

// ErrDeviceUUIDTaken is Cloud's explicit pre-mint UUID collision refusal.
var ErrDeviceUUIDTaken = errors.New("device UUID already in use")

// DeviceUUIDTaken distinguishes UUID/address collisions from name duplicates.
func DeviceUUIDTaken(err error, trailer platform.Metadata) bool {
	var status *platform.RPCStatusError
	if !errors.As(err, &status) || status.Code != 6 {
		return false
	}
	for _, entry := range trailer {
		if entry.Name == cloud.CollidingFieldMetadataKey && (string(entry.Value) == "device_id" || string(entry.Value) == "mesh_suffix") {
			return true
		}
	}
	return false
}

// RetryTakenDeviceUUID retries only a confirmed pre-mint collision, four times.
func RetryTakenDeviceUUID(ctx context.Context, entropy platform.Entropy, id string, enroll func(string) error) error {
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := enroll(id)
		if !errors.Is(err, ErrDeviceUUIDTaken) || attempt == 4 {
			return err
		}
		bytes, e := entropy.RandomBytes(ctx, 16)
		if e != nil {
			return e
		}
		if len(bytes) != 16 {
			return fmt.Errorf("host returned invalid UUID entropy length")
		}
		bytes[6] = bytes[6]&15 | 64
		bytes[8] = bytes[8]&63 | 128
		id = fmt.Sprintf("%x-%x-%x-%x-%x", bytes[:4], bytes[4:6], bytes[6:8], bytes[8:10], bytes[10:])
	}
}
func canonicalUUID(id string) bool {
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return false
	}
	for i, c := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func operatorTenant(principal string) (string, error) {
	rest, ok := strings.CutPrefix(principal, "spiffe://wendy.sh/tenant/")
	if !ok {
		return "", fmt.Errorf("invalid operator principal")
	}
	tenant, kindAndName, ok := strings.Cut(rest, "/")
	if !ok || !canonicalUUID(tenant) {
		return "", fmt.Errorf("invalid operator principal")
	}
	kind, name, ok := strings.Cut(kindAndName, "/")
	if !ok || name == "" {
		return "", fmt.Errorf("invalid operator principal")
	}
	switch kind {
	case "operator":
		return tenant, nil
	case "service":
		if strings.HasPrefix(name, "asset-") || name == "user-" {
			return "", fmt.Errorf("invalid operator principal")
		}
		return tenant, nil
	default:
		return "", fmt.Errorf("invalid operator principal")
	}
}

// PKISiblingHost derives the existing deployment's named PKI frontend.
func PKISiblingHost(endpoint, label string) string {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	rest, ok := strings.CutPrefix(u.Host, "identity.")
	if !ok || rest == "" {
		return ""
	}
	return label + "." + rest
}
func liteBrokerHost(endpoint string) (string, error) {
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("relay endpoint must be an HTTPS URL or host:port")
		}
		endpoint = u.Host
	}
	if strings.ContainsAny(endpoint, "/ ?#@\r\n\t") {
		return "", fmt.Errorf("invalid relay endpoint")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
		port = "443"
	}
	if host == "" || port == "" {
		return "", fmt.Errorf("empty relay endpoint")
	}
	switch host {
	case "api.wendy.sh", "api.wendy.dev":
		return "tunnel.wendy.dev", nil
	case "api.dev.wendy.sh", "api.dev.wendy.dev":
		return "tunnel.dev.wendy.dev", nil
	}
	return host, nil
}
