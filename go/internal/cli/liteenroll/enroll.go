// Package liteenroll prepares an operator-authorized Tier C enrollment.
// The firmware creates its own key and redeems the credential directly at PKI.
package liteenroll

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/cloudrelay"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc"
)

// Config resolves PKI endpoints and the agent's device hostname from the selected
// session. WendyCom uses its own port on that hostname.
func Config(auth *config.AuthConfig, deviceID, csrURL, timeURL, brokerHost string, brokerPort uint32) (*litepb.WendyConfEnrollment, error) {
	if auth == nil || len(auth.Certificates) == 0 {
		return nil, fmt.Errorf("enrollment requires a PKI operator session; run 'wendy cloud login'")
	}
	if brokerHost == "" {
		endpoint, err := cloudrelay.DeviceEndpoint(auth.CloudGRPC, "")
		if err != nil {
			return nil, fmt.Errorf("derive WendyCom broker hostname; pass --broker-host: %w", err)
		}
		brokerHost, _, err = net.SplitHostPort(endpoint)
		if err != nil {
			return nil, fmt.Errorf("derive WendyCom broker hostname; pass --broker-host: %w", err)
		}
	}
	principal, err := certs.ParsePrincipal(auth.Certificates[0].PrincipalURI)
	if err != nil || principal.EntityType != certs.EntityUser {
		return nil, fmt.Errorf("enrollment requires a tenant operator identity")
	}
	tenant := principal.TenantUUID
	if len(deviceID) == 0 || len(deviceID) > 64 || strings.Trim(deviceID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_.") != "" {
		return nil, fmt.Errorf("device ID must contain 1–64 letters, digits, dots, underscores or hyphens")
	}
	if csrURL == "" {
		host := cloudenroll.PKISiblingHost(auth.PKIEndpoint, "csr")
		if host == "" {
			return nil, fmt.Errorf("pass --csr-url for this PKI deployment")
		}
		csrURL = "https://" + host + "/v1/" + tenant
	}
	if timeURL == "" {
		timeURL = "roughtime"
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
func Mint(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, cfg *litepb.WendyConfEnrollment, name string) (string, error) {
	artifact, err := cloudrequest.EnrollmentRequestForClass(auth, cfg.DeviceId, "C")
	if err != nil {
		return "", err
	}
	reply := new(cloudpb.EnrollDeviceResponse)
	err = cloudrequest.Invoke(ctx, conn, auth, cloudpb.DeviceEnrollmentService_EnrollDevice_FullMethodName, &cloudpb.EnrollDeviceRequest{DeviceId: cfg.DeviceId, DeviceClass: cloudpb.DeviceClass_DEVICE_CLASS_C, EnrollmentRequestJws: artifact, Name: name}, reply)
	if err != nil {
		return "", fmt.Errorf("creating Tier C enrollment: %w", err)
	}
	expires, err := time.Parse(time.RFC3339, reply.GetExpiresAt())
	if reply.GetCredentialKind() != "enrollment_token" || reply.GetAssetId() == "" || len(reply.GetTokenValue()) == 0 || len(reply.GetTokenValue()) > 512 || strings.ContainsAny(reply.GetTokenValue(), "\r\n") || err != nil || time.Until(expires) < 30*time.Second {
		return reply.GetAssetId(), fmt.Errorf("Cloud returned an invalid or nearly expired Tier C enrollment credential")
	}
	cfg.Token = reply.GetTokenValue()
	return reply.GetAssetId(), nil
}

// SignedTime obtains a device-nonce-bound timestamp over verified HTTPS. The
// device verifies the CMS signature and pinned TSA chain before using the time.
func SignedTime(ctx context.Context, client *http.Client, endpoint, nonceHex string) ([]byte, error) {
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(der))
	if err != nil {
		return nil, err
	}
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("signed time requires HTTPS")
	}
	req.Header.Set("Content-Type", "application/timestamp-query")
	secureClient := *client
	secureClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := secureClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getting signed PKI time: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
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
