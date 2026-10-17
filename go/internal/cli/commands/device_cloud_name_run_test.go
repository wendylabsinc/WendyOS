package commands

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

func TestRunCloudNameFailureDoesNotRetryThroughUnpinnedSession(t *testing.T) {
	for _, tc := range []struct {
		name           string
		missingSession bool
		refreshError   bool
		want           error
	}{
		{"no matching legacy pin session", true, false, errDeviceIdentityRefused},
		{"not found", false, false, &errCloudDeviceNotFound{name: "ci-mesh-2"}},
		{"offline", false, false, commandErrorf(errDeviceOffline, "device is enrolled but offline")},
		{"discovery failed", false, false, errors.New("Cloud discovery unavailable")},
		{"certificate preparation failed", false, true, errors.New("certificate refresh failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			t.Setenv("WENDY_AGENT_SOCKET", "")
			t.Setenv(runNoCloudFallbackEnv, "")
			deviceFlag = "ci-mesh-2"

			pin := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"}
			cfg := &config.Config{
				CurrentContext: "other",
				Auth:           []config.AuthConfig{{Name: "other", CloudGRPC: "grpc.b.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: 9}}}},
			}
			if tc.missingSession {
				pin.AssetID = ""
			} else {
				cfg.Auth = append(cfg.Auth, config.AuthConfig{Name: "pinned", CloudGRPC: pin.CloudGRPC, Certificates: []config.CertificateInfo{{OrganizationID: pin.OrgID}}})
			}
			cfg.DevicePins = map[string]config.DevicePin{deviceFlag: pin}
			setTempConfig(t, cfg)

			oldLookup, oldBrowse, oldDiscover := osLookupHostFn, lanBrowseFn, discoverLANDevices
			oldUSB, oldFind := usbDirectCandidatesFn, findDeviceByIDFn
			oldCloud, oldFallback := connectUnresolvedCloudNameFn, cloudFallbackConnectFn
			oldFresh, oldUpdate := ensureFreshCertificateFn, checkAndOfferUpdateFn
			oldDial := dialAgentLadderFn
			t.Cleanup(func() {
				osLookupHostFn, lanBrowseFn, discoverLANDevices = oldLookup, oldBrowse, oldDiscover
				usbDirectCandidatesFn, findDeviceByIDFn = oldUSB, oldFind
				connectUnresolvedCloudNameFn, cloudFallbackConnectFn = oldCloud, oldFallback
				ensureFreshCertificateFn, checkAndOfferUpdateFn = oldFresh, oldUpdate
				dialAgentLadderFn = oldDial
			})
			osLookupHostFn = func(context.Context, string) ([]string, error) { return nil, unresolvedCloudNameError() }
			lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
			discoverLANDevices = lanBrowseFn
			usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
			findDeviceByIDFn = func(context.Context, string) *SelectedDevice { return nil }
			// The fixture certificates carry only session metadata. Return the
			// resolver miss without building a real mTLS connection from them.
			dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
				return nil, nil, unresolvedCloudNameError()
			}
			ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error {
				if tc.refreshError {
					return tc.want
				}
				return nil
			}
			cloudCalls, fallbackCalls, updateCalls := 0, 0, 0
			connectUnresolvedCloudNameFn = func(_ context.Context, auth *config.AuthConfig, name, _ string, expected *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				cloudCalls++
				if auth.CloudGRPC != pin.CloudGRPC || cloudAuthOrgID(auth) != int32(pin.OrgID) || name != "42" || expected == nil || expected.EntityID != "42" {
					t.Fatalf("Cloud lookup escaped pin: auth=%s org=%d selector=%q expected=%+v", auth.CloudGRPC, cloudAuthOrgID(auth), name, expected)
				}
				return nil, tc.want
			}
			cloudFallbackConnectFn = func(context.Context, string, string, string, *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				fallbackCalls++
				return nil, errors.New("unpinned Cloud fallback must not run")
			}
			checkAndOfferUpdateFn = func(_ context.Context, conn *grpcclient.AgentConnection) (*grpcclient.AgentConnection, error) {
				updateCalls++
				return conn, nil
			}

			got, err := resolveWithCloudFallback(context.Background(), "", SuppressProvisioningHint(), DisableSessionBroker(), NonInteractive(), ExcludeProviders("local"))
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("connection = %v, %v; want original Cloud error %v", got, err, tc.want)
			}
			wantCloudCalls := 1
			if tc.missingSession || tc.refreshError {
				wantCloudCalls = 0
			}
			if cloudCalls != wantCloudCalls || fallbackCalls != 0 || updateCalls != 0 {
				t.Fatalf("Cloud calls=%d, fallback calls=%d, update calls=%d; want %d, 0, 0", cloudCalls, fallbackCalls, updateCalls, wantCloudCalls)
			}
		})
	}
}
