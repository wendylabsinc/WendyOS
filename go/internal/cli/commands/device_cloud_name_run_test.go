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
		{"no matching legacy pin session", true, false, config.ErrNotLoggedIn},
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
			oldCloud, oldFallback := connectCloudLookupFn, cloudFallbackConnectFn
			oldFresh, oldUpdate := ensureFreshCertificateFn, checkAndOfferUpdateFn
			oldDial := dialAgentLadderFn
			t.Cleanup(func() {
				osLookupHostFn, lanBrowseFn, discoverLANDevices = oldLookup, oldBrowse, oldDiscover
				usbDirectCandidatesFn, findDeviceByIDFn = oldUSB, oldFind
				connectCloudLookupFn, cloudFallbackConnectFn = oldCloud, oldFallback
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
			connectCloudLookupFn = func(_ context.Context, auth *config.AuthConfig, name, _ string, expected *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				cloudCalls++
				if auth.CloudGRPC != pin.CloudGRPC || cloudAuthOrgID(auth) != int32(pin.OrgID) || name != "42" || expected == nil || expected.EntityID != "42" {
					t.Fatalf("Cloud lookup escaped pin: auth=%s org=%d selector=%q expected=%+v", auth.CloudGRPC, cloudAuthOrgID(auth), name, expected)
				}
				return nil, tc.want
			}
			cloudFallbackConnectFn = func(context.Context, *config.Config, string) (*grpcclient.AgentConnection, error) {
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

// A dial that fails without a DNS miss skips the Cloud name lookup and reaches
// the final fallback. A pinned --device, and the default, must still be looked
// up with the pin's session and identity there, not the current session.
func TestRunFinalCloudFallbackUsesPinnedSession(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(map[bool]string{true: "explicit device", false: "default device"}[explicit], func(t *testing.T) {
			restoreDeviceGlobals(t)
			stubNonInteractive(t)
			setPinCache(t)
			t.Setenv("WENDY_AGENT_SOCKET", "")
			t.Setenv(runNoCloudFallbackEnv, "")
			const name = "ci-mesh-2"
			pin := config.DevicePin{OrgID: 7, CloudGRPC: "grpc.a.sh:443", AssetID: "42"}
			cfg := &config.Config{
				CurrentContext: "current",
				Auth: []config.AuthConfig{
					{Name: "current", CloudGRPC: "grpc.b.sh:443", Certificates: []config.CertificateInfo{{OrganizationID: pin.OrgID}}},
					{Name: "pinned", CloudGRPC: pin.CloudGRPC, Certificates: []config.CertificateInfo{{OrganizationID: pin.OrgID}}},
				},
				DevicePins: map[string]config.DevicePin{name: pin},
			}
			if explicit {
				deviceFlag = name
			} else {
				cfg.DefaultDevice = name
			}
			setTempConfig(t, cfg)

			oldLookup, oldBrowse, oldDiscover := osLookupHostFn, lanBrowseFn, discoverLANDevices
			oldUSB, oldFind := usbDirectCandidatesFn, findDeviceByIDFn
			oldCloud, oldFresh, oldDial := connectCloudLookupFn, ensureFreshCertificateFn, dialAgentLadderFn
			t.Cleanup(func() {
				osLookupHostFn, lanBrowseFn, discoverLANDevices = oldLookup, oldBrowse, oldDiscover
				usbDirectCandidatesFn, findDeviceByIDFn = oldUSB, oldFind
				connectCloudLookupFn, ensureFreshCertificateFn, dialAgentLadderFn = oldCloud, oldFresh, oldDial
			})
			timeout := errors.New("dial tcp 192.0.2.10:50052: i/o timeout")
			osLookupHostFn = func(context.Context, string) ([]string, error) { return []string{"192.0.2.10"}, nil }
			lanBrowseFn = func(context.Context, time.Duration) ([]models.LANDevice, error) { return nil, nil }
			discoverLANDevices = lanBrowseFn
			usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
			findDeviceByIDFn = func(context.Context, string) *SelectedDevice { return nil }
			dialAgentLadderFn = func(context.Context, dialTarget) (*grpcclient.AgentConnection, error, error) {
				return nil, nil, timeout
			}
			ensureFreshCertificateFn = func(context.Context, *config.AuthConfig) error { return nil }
			calls := 0
			connectCloudLookupFn = func(_ context.Context, auth *config.AuthConfig, selector, _ string, expected *certs.WendyIdentity) (*grpcclient.AgentConnection, error) {
				calls++
				if auth.Name != "pinned" || selector != pin.AssetID || expected == nil || expected.EntityID != pin.AssetID || expected.OrgID != int32(pin.OrgID) {
					t.Fatalf("Cloud lookup = session %q, selector %q, expected %+v; want the pinned session and asset", auth.Name, selector, expected)
				}
				return nil, errors.New("device is offline")
			}

			got, err := resolveWithCloudFallback(context.Background(), "", SuppressProvisioningHint(), DisableSessionBroker(), NonInteractive(), ExcludeProviders("local"))
			if got != nil || err == nil || calls != 1 {
				t.Fatalf("connection = %v, %v, Cloud lookups = %d; want one pinned lookup", got, err, calls)
			}
		})
	}
}
