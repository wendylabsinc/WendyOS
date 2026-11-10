package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

func TestCloudSelectorCertificateExpiresDuringRenewal(t *testing.T) {
	const tenant = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const assetID = "7791d76e-a942-4a8e-b582-063d063b5623"
	for _, tc := range []struct {
		name           string
		issuer         string
		serviceAccount string
		wantRefresh    bool
	}{
		{name: "OAuth", issuer: "https://auth.example", wantRefresh: true},
		{name: "non-OAuth"},
		{name: "service account", issuer: "https://auth.example", serviceAccount: "robot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetAutomaticCertReissue(t)
			now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
			expiry := now.Add(time.Second)
			auth := config.AuthConfig{
				CloudGRPC: "cloud.example:443", OAuthIssuer: tc.issuer,
				ServiceAccount: tc.serviceAccount, RefreshToken: "original",
				Certificates: []config.CertificateInfo{{
					PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test", PemCertificate: "expiring",
				}},
			}
			setTempConfig(t, &config.Config{Auth: []config.AuthConfig{auth}})
			t.Setenv(renewEndpointEnv, "https://renew.example/v1/renew")
			oldNow, oldExpiry, oldRenew := timeNowFn, leafNotAfterFn, renewViaPKICore
			oldRefresh := refreshDefaultCloudCertificateFn
			oldFetch, oldConnect := fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn
			t.Cleanup(func() {
				timeNowFn, leafNotAfterFn, renewViaPKICore = oldNow, oldExpiry, oldRenew
				refreshDefaultCloudCertificateFn = oldRefresh
				fetchDefaultCloudAssetsV2Fn, connectDefaultCloudAssetV2Fn = oldFetch, oldConnect
			})
			timeNowFn = func() time.Time { return now }
			leafNotAfterFn = func(cert string) (time.Time, error) {
				if cert == "fresh" {
					return expiry.Add(time.Hour), nil
				}
				return expiry, nil
			}
			renewCalls, refreshCalls := 0, 0
			renewViaPKICore = func(context.Context, string, *config.AuthConfig) (string, string, string, error) {
				renewCalls++
				now = expiry.Add(time.Second)
				return "", "", "", errors.New("renewal transport failed")
			}
			refreshDefaultCloudCertificateFn = func(_ context.Context, selected *config.AuthConfig) error {
				refreshCalls++
				selected.Certificates[0].PemCertificate = "fresh"
				selected.RefreshToken = "rotated"
				return nil
			}
			discoveredWith, connectedWith := "", ""
			fetchDefaultCloudAssetsV2Fn = func(_ context.Context, selected *config.AuthConfig, _ bool) ([]*cloudpbv2.Asset, error) {
				discoveredWith = selected.Certificates[0].PemCertificate
				return []*cloudpbv2.Asset{{Id: assetID}}, nil
			}
			connectDefaultCloudAssetV2Fn = func(_ context.Context, selected *config.AuthConfig, _ *cloudpbv2.Asset, _ string) (*grpcclient.AgentConnection, error) {
				connectedWith = selected.Certificates[0].PemCertificate
				return &grpcclient.AgentConnection{}, nil
			}
			_, err := connectCloudDeviceSelector(context.Background(), cloudDeviceSelector{Endpoint: auth.CloudGRPC, TenantUUID: tenant, AssetUUID: assetID})
			if renewCalls != 1 {
				t.Fatalf("renewal calls = %d, want 1", renewCalls)
			}
			if tc.wantRefresh {
				if err != nil || refreshCalls != 1 || discoveredWith != "fresh" || connectedWith != "fresh" {
					t.Fatalf("expired certificate was not replaced before discovery/tunnel: err=%v, refreshes=%d, discovery=%q, tunnel=%q", err, refreshCalls, discoveredWith, connectedWith)
				}
				cfg, err := config.Load()
				if err != nil || cfg.Auth[0].RefreshToken != "rotated" || cfg.Auth[0].Certificates[0].PemCertificate != "fresh" {
					t.Fatalf("refreshed credentials were not persisted: %v", err)
				}
			} else if !errors.Is(err, errCertExpired) || refreshCalls != 0 || discoveredWith != "" || connectedWith != "" {
				t.Fatalf("expired certificate reached discovery/tunnel: err=%v, refreshes=%d, discovery=%q, tunnel=%q", err, refreshCalls, discoveredWith, connectedWith)
			}
		})
	}
}

func TestPrepareDefaultCloudCertificateWithoutReadableLeaf(t *testing.T) {
	for _, tc := range []struct {
		name string
		auth *config.AuthConfig
	}{
		{name: "nil session"},
		{name: "missing certificate", auth: &config.AuthConfig{}},
		{name: "malformed certificate", auth: &config.AuthConfig{Certificates: []config.CertificateInfo{{PemCertificate: "invalid"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			if tc.auth != nil {
				cfg.Auth = []config.AuthConfig{*tc.auth}
			}
			setTempConfig(t, cfg)
			if err := prepareDefaultCloudCertificate(context.Background(), tc.auth); err != nil {
				t.Fatalf("prepareDefaultCloudCertificate() = %v, want the connection to diagnose the missing or unreadable certificate", err)
			}
		})
	}
}
