package commands

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/buildassignment"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

// authorizeRemoteBuild finishes every grant before submitting the build. The
// public builder bundle is verified independently of the submission tunnel;
// the expected principal comes from the device connection's existing binding.
func authorizeRemoteBuild(ctx context.Context, builder *grpcclient.AgentConnection, spec *agentpbv2.BuildSpec, destinations map[string]*grpcclient.AgentConnection) error {
	capabilities, err := builder.BuildService.GetBuildCapabilities(ctx, &agentpbv2.GetBuildCapabilitiesRequest{})
	if err != nil {
		return err
	}
	if !capabilities.GetAuthorizedDelivery() {
		return fmt.Errorf("update the build host: it does not support operator-authorized delivery")
	}
	public, err := agentpbv2.NewWendyBuildDeliveryServiceClient(builder.Conn).GetDeliveryCapabilities(ctx, &agentpbv2.GetDeliveryCapabilitiesRequest{})
	if err != nil {
		return fmt.Errorf("getting build host public credentials: %w", err)
	}
	id, verified := builder.ObservedServerIdentity()
	expected, err := meshid.DeviceFromPrincipal(id.Principal)
	if !verified || err != nil || builder.CertInfo == nil {
		return fmt.Errorf("verified build host identity or operator trust unavailable")
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
	targets := spec.GetPushTargets()
	if spec.GetPushTarget() != nil {
		targets = []*agentpbv2.PushTarget{spec.GetPushTarget()}
	}
	for _, target := range targets {
		conn := destinations[target.GetAssetUuid()]
		if conn == nil {
			return fmt.Errorf("operator connection missing for delivery target %s", target.GetAssetUuid())
		}
		if err := authorizeBuildTarget(ctx, conn, target, expected, leaf, input); err != nil {
			return fmt.Errorf("authorizing build delivery to %s: %w", target.GetAssetUuid(), err)
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

func authorizeBuildTarget(ctx context.Context, conn *grpcclient.AgentConnection, target *agentpbv2.PushTarget, builder meshid.Device, builderLeaf *x509.Certificate, input string) error {
	service := agentpbv2.NewWendyBuildDeliveryServiceClient(conn.Conn)
	public, err := service.GetDeliveryCapabilities(ctx, &agentpbv2.GetDeliveryCapabilitiesRequest{})
	if err != nil {
		return fmt.Errorf("target must support operator-authorized delivery: %w", err)
	}
	id, verified := conn.ObservedServerIdentity()
	device, err := meshid.DeviceFromPrincipal(id.Principal)
	if !verified || err != nil || device.UUID().String() != target.GetAssetUuid() || device.Tenant() != builder.Tenant() || public.GetTargetPrincipal() != device.Principal() || public.GetContractVersion() != builddelivery.Version {
		return fmt.Errorf("delivery target does not match the authenticated device and tenant")
	}
	root, err := deliveryTrust(conn.CertInfo)
	if err != nil {
		return err
	}
	credential, err := deliveryCredential(ctx, conn, device, false)
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
	claims := builddelivery.Claims{Version: builddelivery.Version, JobID: uuid.NewString(), Operator: credential.PrincipalURI, Target: device.Principal(), Builder: builder.Principal(), BuilderSPKI: builddelivery.Fingerprint(builderLeaf), Reference: target.GetRepository(), InputSHA256: input, ExpectedDigest: previous.GetDigest(), IssuedAt: now.Unix(), AdmitBefore: admitBefore, ValidUntil: now.Add(builddelivery.MaxLifetime).Unix(), Operations: []string{"stage", "publish"}, MaxBytes: 64 << 30}
	key, err := credential.PrivateKeyPEM()
	if err != nil {
		return err
	}
	compact, err := builddelivery.Sign(claims, credential.PemCertificate, credential.PemCertificateChain, key)
	if err != nil {
		return err
	}
	// Also validate a Cloud response locally, against pre-existing trust and
	// the actual entitlement extension, before handing it to the target.
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

// deliveryCredential reuses a still-valid, device-specific credential prepared
// before an airgap. Only public artifacts reach the worker or target; this
// ephemeral signing key stays in the operator's credential store.
func deliveryCredential(ctx context.Context, conn *grpcclient.AgentConnection, device meshid.Device, refresh bool) (*config.CertificateInfo, error) {
	if conn.CertInfo == nil {
		return nil, fmt.Errorf("operator certificate unavailable")
	}
	operator := conn.CertInfo.CertificatePrincipal()
	if !refresh {
		if credential, err := loadDeliveryCredential(operator, device); err == nil {
			return credential, nil
		}
	}
	response, err := (deploymentProvisioningService{conn}).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	auth, err := deploymentAuth(cfg, response.GetProvisioned())
	if err != nil {
		return nil, err
	}
	if auth.Certificates[0].CertificatePrincipal() != operator {
		return nil, fmt.Errorf("Cloud session disagrees with the connection operator")
	}
	cloud, err := dialCloudGRPC(auth)
	if err != nil {
		return nil, err
	}
	defer cloud.Close()
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cloudCtx, err := cloudContext(bounded, auth)
	if err != nil {
		return nil, err
	}
	key, err := certs.GenerateMLDSAKeyPair()
	if err != nil {
		return nil, err
	}
	csr, err := certs.GenerateCSR([]byte(key), "operator", []string{operator})
	if err != nil {
		return nil, err
	}
	ttl := int64(900)
	out := &cloudpbv2.RequestEntitledCertificateResponse{}
	err = cloudrequest.Invoke(cloudCtx, cloud, auth, cloudpbv2.OperatorCertificateService_RequestEntitledCertificate_FullMethodName, &cloudpbv2.RequestEntitledCertificateRequest{OrganizationId: device.Tenant().String(), CsrPem: csr, TargetType: cloudpbv2.AccessTargetType_ACCESS_TARGET_TYPE_DEVICE, TargetRef: device.UUID().String(), TtlSeconds: &ttl}, out)
	if err != nil {
		return nil, fmt.Errorf("obtaining deployment permission: %w; for an airgap, run 'wendy auth prepare-build-delivery --device <target>' while connected, then admit the job before that credential expires", err)
	}
	credential := &config.CertificateInfo{PemCertificate: out.GetCertificatePem(), PemCertificateChain: out.GetChainPem(), PemPrivateKey: key, PrincipalURI: operator}
	// Match the requested key and identity; grant authorization is verified
	// independently against local roots immediately before admission.
	if _, err := certs.TLSKeyPair(credential.PemCertificate, credential.PemCertificateChain, key); err != nil || credential.CertificatePrincipal() != operator {
		return nil, fmt.Errorf("Cloud returned a mismatched delivery credential")
	}
	pair, err := certs.TLSKeyPair(credential.PemCertificate, credential.PemCertificateChain, key)
	if err != nil {
		return nil, err
	}
	roots, err := deliveryTrust(conn.CertInfo)
	if err != nil {
		return nil, err
	}
	if err := builddelivery.VerifyAuthority(pair.Certificate, roots, device, operator, time.Now()); err != nil {
		return nil, fmt.Errorf("verifying issued deployment authority: %w", err)
	}
	if err := saveDeliveryCredential(operator, device, credential); err != nil {
		return nil, err
	}
	return credential, nil
}
