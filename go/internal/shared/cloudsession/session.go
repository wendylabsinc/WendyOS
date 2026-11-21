// Package cloudsession provides saved-account selection and authenticated Cloud
// connections for app and CLI callers. It has no notification or UI policy.
package cloudsession

import (
	"context"
	"crypto"
	"errors"
	"fmt"

	"github.com/wendylabsinc/wendy/go/internal/cli/clouddefaults"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/failure"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

// RefreshAccessToken reuses the caller's existing persisted OAuth refresh path.
// It must update only the supplied session snapshot.
type RefreshAccessToken func(context.Context, *config.AuthConfig) error

// LoadAccount selects an explicit saved account, optionally enforcing its tenant.
func LoadAccount(account, organization string) (*config.AuthConfig, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	selected, ok := cfg.ContextByName(account)
	if !ok {
		return nil, failure.Wrap(errors.New("This saved account is no longer available. Sign in again."), failure.AuthenticationRequired, failure.SignIn)
	}
	if organization == "" {
		return selected, nil
	}
	if len(selected.Certificates) > 0 && selected.Certificates[0].TenantUUID() == organization {
		return selected, nil
	}

	return nil, failure.Wrap(errors.New("Sign in to this organization first, then refresh accounts."), failure.AuthenticationRequired, failure.SignIn)
}

// LegacyCertificateHeader preserves legacy Cloud identity headers.
func LegacyCertificateHeader(cert config.CertificateInfo) string {
	if cert.PrincipalURI != "" {
		return ""
	}
	if cert.UserID != "" {
		return fmt.Sprintf("URI=urn:wendy:org:%d:user:%s", cert.OrganizationID, cert.UserID)
	}
	if cert.AssetID != 0 {
		return fmt.Sprintf("URI=urn:wendy:org:%d:asset:%d", cert.OrganizationID, cert.AssetID)
	}
	return ""
}

// Context authenticates a request without mutating the shared saved account.
func Context(ctx context.Context, auth *config.AuthConfig, refresh RefreshAccessToken) (context.Context, error) {
	if auth.OAuthIssuer != "" {
		// Discovery can build several RPC contexts concurrently from one auth
		// entry. Refresh a local snapshot rather than mutating shared state.
		local := *auth
		auth = &local
		if err := refresh(ctx, auth); err != nil {
			return nil, err
		}
	}
	md := metadata.MD{}
	// A DPoP-bound token (OAuth login) must not go out as Bearer — the DPoP
	// interceptor installed by Dial sets
	// `authorization: DPoP <token>` plus a per-call proof. Only unbound
	// API-key/legacy sessions carry a Bearer here (WDY-3107).
	if auth.HasAPIKey() && !cloudrequest.IsDPoPBound(auth) {
		bearerToken, err := auth.BearerToken()
		if err != nil {
			return nil, fmt.Errorf("loading API token: %w", err)
		}
		md.Set("authorization", "Bearer "+bearerToken)
	}
	if len(auth.Certificates) > 0 {
		certHeader := LegacyCertificateHeader(auth.Certificates[0])
		if certHeader != "" {
			md.Set("x-wendy-client-cert", certHeader)
			md.Set("x-forwarded-client-cert", certHeader)
		}
	}
	return metadata.NewOutgoingContext(ctx, md), nil
}

// Dial creates the shared TLS/DPoP Cloud connection.
func Dial(auth *config.AuthConfig, refresh RefreshAccessToken) (*grpc.ClientConn, error) {
	var cert *config.CertificateInfo
	if len(auth.Certificates) > 0 {
		cert = &auth.Certificates[0]
	}
	creds, err := clouddefaults.CloudTransportCredentials(auth.CloudGRPC, cert)
	if err != nil {
		return nil, err
	}
	dialOptions := append([]grpc.DialOption{
		grpc.WithTransportCredentials(creds),
		grpc.WithInitialWindowSize(8 * 1024 * 1024),
		grpc.WithInitialConnWindowSize(16 * 1024 * 1024),
		grpc.WithReadBufferSize(256 * 1024),
		grpc.WithWriteBufferSize(256 * 1024),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                clouddefaults.KeepalivePing,
			Timeout:             clouddefaults.KeepaliveACKTimeout,
			PermitWithoutStream: true,
		}),
	}, cloudrequest.DPoPDialOptions(auth, TokenProvider(auth, refresh))...)
	conn, err := grpc.NewClient(auth.CloudGRPC, dialOptions...)
	if err != nil {
		return nil, fmt.Errorf("connecting to cloud: %w", err)
	}
	return conn, nil
}

// TokenProvider refreshes a private snapshot for each authenticated RPC.
func TokenProvider(auth *config.AuthConfig, refresh RefreshAccessToken) cloudrequest.DPoPTokenProvider {
	return func(ctx context.Context) (string, crypto.Signer, error) {
		local := *auth
		local.Certificates = append([]config.CertificateInfo(nil), auth.Certificates...)
		if err := refresh(ctx, &local); err != nil {
			return "", nil, err
		}
		keyPEM, err := local.OAuthDPoPKey()
		if err != nil {
			return "", nil, fmt.Errorf("DPoP: loading bound key: %w", err)
		}
		key, err := certs.ParseSigningPrivateKeyPEM([]byte(keyPEM))
		if err != nil {
			return "", nil, fmt.Errorf("DPoP: parsing bound key: %w", err)
		}
		return local.APIKey, key, nil
	}
}

// With closes its connection after the authenticated operation completes.
func With(ctx context.Context, auth *config.AuthConfig, refresh RefreshAccessToken, call func(context.Context, grpc.ClientConnInterface) error) error {
	conn, err := Dial(auth, refresh)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	ctx, err = Context(ctx, auth, refresh)
	if err != nil {
		return err
	}
	return call(ctx, conn)
}
