package commands

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/buildassignment"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type buildDeliveryDestination struct {
	target     *agentpbv2.PushTarget
	device     meshid.Device
	credential *config.CertificateInfo
	service    agentpbv2.WendyBuildDeliveryServiceClient
}

// authorizeRemoteBuild selects one delivery mode for the entire assignment
// before registering any grants. Only explicit lack of support on both sides
// permits the legacy mode.
func authorizeRemoteBuild(ctx context.Context, builder *grpcclient.AgentConnection, spec *agentpbv2.BuildSpec, destinations map[string]*grpcclient.AgentConnection) error {
	capabilities, err := builder.BuildService.GetBuildCapabilities(ctx, &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		return fmt.Errorf("checking build host delivery support: %w", err)
	}
	builderSupportsDelivery := capabilities.GetAuthorizedDelivery()
	id, verified := builder.ObservedServerIdentity()
	expected, err := meshid.DeviceFromPrincipal(id.Principal)
	if !verified || err != nil || builder.CertInfo == nil {
		return fmt.Errorf("verified build host identity or operator trust unavailable")
	}

	targets := spec.GetPushTargets()
	if spec.GetPushTarget() != nil {
		targets = []*agentpbv2.PushTarget{spec.GetPushTarget()}
	}
	if len(targets) == 0 {
		return fmt.Errorf("no remote build delivery targets")
	}
	authorizedTargets := make([]buildDeliveryDestination, 0, len(targets))
	for _, target := range targets {
		assetID := target.GetAssetUuid()
		conn := destinations[assetID]
		if conn == nil {
			return fmt.Errorf("operator connection missing for delivery target %s", assetID)
		}
		service := agentpbv2.NewWendyBuildDeliveryServiceClient(conn.Conn)
		public, err := service.GetDeliveryCapabilities(ctx, &agentpbv2.GetDeliveryCapabilitiesRequest{})
		targetSupportsDelivery := err == nil
		if err != nil && status.Code(err) != codes.Unimplemented {
			return fmt.Errorf("checking operator-authorized delivery on target %s: %w", assetID, err)
		}
		id, verified := conn.ObservedServerIdentity()
		device, err := meshid.DeviceFromPrincipal(id.Principal)
		if !verified || err != nil || device.UUID().String() != assetID || device.Tenant() != expected.Tenant() {
			return fmt.Errorf("delivery target %s does not match the authenticated device and tenant", assetID)
		}
		switch {
		case builderSupportsDelivery && !targetSupportsDelivery:
			return fmt.Errorf("update target %s: it does not support operator-authorized delivery", assetID)
		case !builderSupportsDelivery && targetSupportsDelivery:
			return fmt.Errorf("update the build host: target %s supports operator-authorized delivery", assetID)
		case targetSupportsDelivery:
			if public.GetTargetPrincipal() != device.Principal() || public.GetContractVersion() != builddelivery.Version {
				return fmt.Errorf("target %s reports a different delivery identity or unsupported contract", assetID)
			}
			authorizedTargets = append(authorizedTargets, buildDeliveryDestination{
				target: target, device: device, credential: conn.CertInfo, service: service,
			})
		}
	}
	if !builderSupportsDelivery {
		return nil
	}

	// Verify the public builder bundle independently of the submission tunnel.
	public, err := agentpbv2.NewWendyBuildDeliveryServiceClient(builder.Conn).GetDeliveryCapabilities(ctx, &agentpbv2.GetDeliveryCapabilitiesRequest{})
	if err != nil {
		return fmt.Errorf("getting build host public credentials: %w", err)
	}
	roots, err := deliveryTrust(builder.CertInfo)
	if err != nil {
		return err
	}
	if public.GetContractVersion() != builddelivery.Version || public.GetTargetPrincipal() != expected.Principal() {
		return fmt.Errorf("build host reports a different delivery identity or unsupported contract")
	}
	leaf, err := builddelivery.VerifyBuilderCertificate(public.GetCertificateChain(), roots, expected, time.Now())
	if err != nil {
		return fmt.Errorf("verifying build host authorization key: %w", err)
	}
	input, err := buildassignment.InputSHA256(spec)
	if err != nil {
		return err
	}
	for _, destination := range authorizedTargets {
		if err := destination.authorize(ctx, expected, leaf, input); err != nil {
			return fmt.Errorf("authorizing build delivery to %s: %w", destination.target.GetAssetUuid(), err)
		}
	}
	return nil
}

func deliveryTrust(info *config.CertificateInfo) ([]*x509.Certificate, error) {
	if info == nil {
		return nil, fmt.Errorf("operator PKI trust unavailable")
	}
	chain, err := certs.ParseCertsFromPEM([]byte(info.PemCertificateChain))
	if err != nil {
		return nil, err
	}
	var roots []*x509.Certificate
	for _, ca := range chain {
		if ca.IsCA && ca.BasicConstraintsValid && (ca.KeyUsage == 0 || ca.KeyUsage&x509.KeyUsageCertSign != 0) {
			roots = append(roots, ca)
		}
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("operator has no locally trusted certificate authorities")
	}
	return roots, nil
}

func (destination buildDeliveryDestination) authorize(ctx context.Context, builder meshid.Device, builderLeaf *x509.Certificate, input string) error {
	target, device, credential, service := destination.target, destination.device, destination.credential, destination.service
	root, err := deliveryTrust(credential)
	if err != nil {
		return err
	}
	previous, err := service.GetDeliveryReference(ctx, &agentpbv2.GetDeliveryReferenceRequest{Reference: target.GetRepository()})
	if err != nil {
		return err
	}
	now := time.Now().Truncate(time.Second)
	chain, err := certs.ParseCertsFromPEM([]byte(credential.PemCertificate))
	if err != nil || len(chain) == 0 {
		return fmt.Errorf("invalid operator delivery certificate")
	}
	admitBefore := min(now.Add(5*time.Minute).Unix(), chain[0].NotAfter.Unix())
	claims := builddelivery.Claims{Version: builddelivery.Version, JobID: uuid.NewString(), Operator: credential.CertificatePrincipal(), Target: device.Principal(), Builder: builder.Principal(), BuilderSPKI: builddelivery.Fingerprint(builderLeaf), Reference: target.GetRepository(), InputSHA256: input, ExpectedDigest: previous.GetDigest(), IssuedAt: now.Unix(), AdmitBefore: admitBefore, ValidUntil: now.Add(builddelivery.MaxLifetime).Unix(), Operations: []string{"stage", "publish"}, MaxBytes: 64 << 30}
	key, err := credential.PrivateKeyPEM()
	if err != nil {
		return err
	}
	compact, err := builddelivery.Sign(claims, credential.PemCertificate, credential.PemCertificateChain, key)
	if err != nil {
		return err
	}
	// Verify the existing operator credential against local trust before
	// handing its signed delivery grant to the target.
	if _, err := builddelivery.Admit(compact, root, device, now); err != nil {
		return fmt.Errorf("operator credential cannot authorize this delivery: %w", err)
	}
	admitted, err := service.AdmitDelivery(ctx, &agentpbv2.AdmitDeliveryRequest{AuthorizationJws: compact})
	if err != nil {
		return err
	}
	if _, err := builddelivery.VerifyBuildBinding(compact, admitted.GetSession(), input, root, device, builderLeaf, now); err != nil {
		return fmt.Errorf("target returned an invalid admission reference: %w", err)
	}
	target.DeliveryAuthorizationJws, target.DeliverySession = compact, admitted.GetSession()
	return nil
}
