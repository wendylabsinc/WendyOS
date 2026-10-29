package commands

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudunenroll"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

var resolveUnenrollTargetFn = resolveTarget

// Resolve cloud defaults before ordinary device resolution, which would require
// an online device and open a tunnel. Removing a cloud enrollment needs neither.
func cloudUnenrollTarget(cmd *cobra.Command, cloudOnly bool) (string, bool, error) {
	selector := deviceFlag
	_, cloudCommand := cloudDeviceConfigFromContext(cmd.Context())
	if cfg, ok := cloudDeviceConfigFromContext(cmd.Context()); ok && cfg.DeviceName != "" {
		selector = cfg.DeviceName
	}
	if selector == "" {
		cfg, err := config.Load()
		if err != nil {
			return "", false, err
		}
		// A local default is meaningful for --cloud-only when it is also the
		// enrolled name. The cloud group uses only cloud-scoped defaults.
		if !cloudCommand || strings.HasPrefix(cfg.DefaultDevice, "cloud:") {
			selector = cfg.DefaultDevice
		}
	}
	_, scoped, err := parseCloudDeviceSelector(selector)
	if err != nil {
		return "", false, err
	}
	return selector, cloudOnly || cloudCommand || scoped, nil
}

func resolveCloudUnenrollAuth(cloudGRPC, selector string) (*config.AuthConfig, string, error) {
	if scoped, matched, err := parseCloudDeviceSelector(selector); matched {
		if err != nil {
			return nil, "", err
		}
		if cloudGRPC != "" && cloudGRPC != scoped.Endpoint {
			return nil, "", fmt.Errorf("--cloud-grpc conflicts with the selected device's cloud endpoint")
		}
		cfg, err := config.Load()
		if err != nil {
			return nil, "", err
		}
		auth, err := scoped.auth(cfg)
		if scoped.AssetUUID != "" {
			return auth, scoped.AssetUUID, err
		}
		return auth, fmt.Sprint(scoped.AssetID), err
	}
	auth, err := resolveAuthEntry(cloudGRPC)
	return auth, selector, err
}

var resolveCloudUnenrollAuthFn = resolveCloudUnenrollAuth

var errCloudEnrollmentNotFound = errors.New("no cloud enrollment")

// Match a local Lite board only by its reported PKI identity or, when it
// reports none, by the hardware id its Lite asset keeps as the serial number.
// An enrolled name may differ from the name still stored on the board.
func resolveUnenrollDevice(devices []cloudDiscoveryDevice, selector, deviceID, hardwareID string) (cloudDiscoveryDevice, error) {
	identity := deviceID
	if hardwareID != "" {
		identity = hardwareID
	}
	if identity == "" {
		if selector == "" {
			return resolveCloudDiscoveryDevice(devices, selector)
		}
		for _, device := range devices {
			if strings.EqualFold(device.key, selector) {
				return device, nil
			}
		}
		identity = selector
	}
	var matches []cloudDiscoveryDevice
	for _, device := range devices {
		identityMatch := device.v2 != nil && cloudunenroll.DeviceID(device.v2) == identity
		if hardwareID != "" {
			identityMatch = device.v2 != nil && isLiteCloudAsset(device.v2) && device.v2.GetSerialNumber() == hardwareID
		}
		nameMatch := deviceID == "" && hardwareID == "" && strings.EqualFold(device.GetName(), selector)
		if identityMatch || nameMatch {
			matches = append(matches, device)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return cloudDiscoveryDevice{}, fmt.Errorf("%w matches %q in the selected organization; use 'wendy cloud discover --all --json' to find its asset ID", errCloudEnrollmentNotFound, identity)
	}
	if hardwareID != "" {
		// Reserved enrollments the board never completed share its serial, and
		// the board holds no identity naming one, so refuse rather than guess.
		ids := make([]string, len(matches))
		for i, device := range matches {
			ids[i] = device.key
		}
		return cloudDiscoveryDevice{}, fmt.Errorf("multiple cloud enrollments (assets %s) carry board %q's hardware id; none was removed. Remove each with 'wendy device unenroll --cloud-only --device <asset-id>', then run 'wendy device unenroll' over USB again to clear the board", strings.Join(ids, ", "), hardwareID)
	}
	return cloudDiscoveryDevice{}, fmt.Errorf("multiple cloud enrollments match %q; select the exact asset ID with --device", identity)
}

func runCloudDeviceUnenroll(cmd *cobra.Command, cloudGRPC, selector, deviceID string, assumeYes bool) error {
	return runCloudDeviceUnenrollAndReset(cmd, cloudGRPC, selector, deviceID, "", "", assumeYes, nil)
}

// Local recovery may clear an identity after a previous cloud-only removal.
// Its absence must be established by a complete, authenticated asset inventory.
func runCloudDeviceUnenrollWithReset(cmd *cobra.Command, cloudGRPC, deviceID string, assumeYes bool, reset func() error) error {
	return runCloudDeviceUnenrollWithResetForTenant(cmd, cloudGRPC, deviceID, "", "", assumeYes, reset)
}

// New USB firmware reports its configured tenant, even for a pending issuance.
// Its reset may use an absent asset only within that same authenticated tenant.
// A board reporting no identity is found by hardwareID, its asset's serial.
func runCloudDeviceUnenrollWithResetForTenant(cmd *cobra.Command, cloudGRPC, deviceID, tenantID, hardwareID string, assumeYes bool, reset func() error) error {
	if (deviceID == "") == (hardwareID == "") || reset == nil {
		return fmt.Errorf("local unenrollment requires one device identity and a reset operation")
	}
	return runCloudDeviceUnenrollAndReset(cmd, cloudGRPC, "", deviceID, hardwareID, tenantID, assumeYes, reset)
}

func fetchUnenrollAssets(ctx context.Context, client cloudpbv2.AssetServiceClient, tenant string) ([]cloudDiscoveryDevice, error) {
	var devices []cloudDiscoveryDevice
	var total int32
	seen := make(map[string]bool)
	// Do not filter by compute-device or online status: an incomplete or
	// disconnected enrollment must still be revoked before resetting the board.
	for offset := int32(0); ; {
		stream, err := client.ListAssets(ctx, &cloudpbv2.ListAssetsRequest{
			OrganizationId: tenant, Offset: int32Ptr(offset), Limit: int32Ptr(200),
		})
		if err != nil {
			return nil, fmt.Errorf("listing cloud enrollments: %w", err)
		}
		var count int32
		for {
			response, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, fmt.Errorf("listing cloud enrollments: %w", err)
			}
			asset := response.GetAsset()
			if asset == nil || asset.GetOrganizationId() != tenant {
				return nil, fmt.Errorf("cloud asset inventory contains an asset outside the selected organization")
			}
			if asset.GetId() == "" || seen[asset.GetId()] {
				return nil, fmt.Errorf("cloud asset inventory contains a missing or repeated asset ID; retry unenrollment")
			}
			seen[asset.GetId()] = true
			if len(devices) >= maxCloudAssets {
				return nil, fmt.Errorf("cloud returned more than %d assets", maxCloudAssets)
			}
			devices = append(devices, cloudDiscoveryDevice{cloudAssetMetadata: asset, v2: asset, key: asset.GetId()})
			count++
			total = max(total, response.GetTotal())
		}
		offset += count
		if count == 0 && offset < total {
			return nil, fmt.Errorf("cloud asset inventory ended before all assets were returned; retry unenrollment")
		}
		if count == 0 || offset >= total {
			return devices, nil
		}
	}
}

func runCloudDeviceUnenrollAndReset(cmd *cobra.Command, cloudGRPC, selector, deviceID, hardwareID, expectedTenant string, assumeYes bool, reset func() error) error {
	if cfg, ok := cloudDeviceConfigFromContext(cmd.Context()); ok && cloudGRPC == "" {
		cloudGRPC = cfg.CloudGRPC
	}
	auth, selector, err := resolveCloudUnenrollAuthFn(cloudGRPC, strings.TrimSpace(selector))
	if err != nil {
		return err
	}
	auth, err = prepareEnrollmentAuth(cmd.Context(), auth)
	if err != nil {
		return err
	}
	if expectedTenant != "" && (len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() != expectedTenant) {
		return fmt.Errorf("the board's configured tenant differs from the selected cloud organization; select its tenant before removing the enrollment")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Minute)
	defer cancel()
	conn, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer conn.Close()
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	label := deviceID
	if hardwareID != "" {
		label = hardwareID
	}
	var device cloudDiscoveryDevice
	assetAbsent := false
	if reset != nil {
		if len(auth.Certificates) == 0 || auth.Certificates[0].TenantUUID() == "" {
			return fmt.Errorf("local Lite unenrollment requires an operator session; run 'wendy auth login'")
		}
		devices, err := fetchUnenrollAssets(cloudCtx, cloudpbv2.NewAssetServiceClient(conn), auth.Certificates[0].TenantUUID())
		if err != nil {
			return err
		}
		device, err = resolveUnenrollDevice(devices, "", deviceID, hardwareID)
		if errors.Is(err, errCloudEnrollmentNotFound) {
			assetAbsent = true
		} else if err != nil {
			return err
		}
	} else if _, parseErr := uuid.Parse(selector); parseErr == nil && len(auth.Certificates) > 0 && auth.Certificates[0].TenantUUID() != "" {
		// Exact UUID selection also works for incomplete enrollments that a
		// compute-device listing might exclude. Check the tenant before mutation.
		asset, err := cloudpbv2.NewAssetServiceClient(conn).GetAsset(cloudCtx, &cloudpbv2.GetAssetRequest{Id: selector})
		if err != nil {
			return fmt.Errorf("reading cloud asset %s: %w", selector, err)
		}
		device = cloudDiscoveryDevice{cloudAssetMetadata: asset, v2: asset, key: asset.GetId()}
	} else {
		// Seeding here would pin the name of the asset this deletes.
		devices, err := listCloudDiscoveryDevices(ctx, auth, false)
		if err != nil {
			return err
		}
		device, err = resolveUnenrollDevice(devices, selector, deviceID, "")
		if err != nil {
			return err
		}
	}
	if device.v2 != nil {
		if device.v2.GetOrganizationId() != auth.Certificates[0].TenantUUID() {
			return fmt.Errorf("cloud asset does not belong to the selected organization")
		}
		if cloudunenroll.DeviceID(device.v2) == "" {
			return fmt.Errorf("asset %s has no device enrollment binding", device.key)
		}
	}
	if !assumeYes {
		if !isInteractiveTerminal() {
			if reset != nil {
				return fmt.Errorf("unenroll is destructive; pass --yes to confirm removing the enrollment for %q in %s and clearing its local identity", label, auth.CloudGRPC)
			}
			return fmt.Errorf("unenroll is destructive; pass --yes to confirm removing %q (asset %s) from %s", device.GetName(), device.key, auth.CloudGRPC)
		}
		if assetAbsent {
			fmt.Fprintf(cmd.OutOrStdout(), "No cloud enrollment for %q exists in the selected organization in %s. This will clear the connected board's stored enrollment identity. WiFi settings and apps will be preserved.\n", label, auth.CloudGRPC)
		} else if reset != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "This will revoke the cloud enrollment for %q (asset %s) in %s, delete its asset record, then clear the connected board's stored enrollment identity. WiFi settings and apps will be preserved.\n", device.GetName(), device.key, auth.CloudGRPC)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "This will revoke the cloud enrollment for %q (asset %s) in %s and delete its asset record. The device will not be contacted or reset.\n", device.GetName(), device.key, auth.CloudGRPC)
		}
		if !confirmDefaultNoFn("Continue?") {
			fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
			return nil
		}
	}
	var result cloudunenroll.Result
	if assetAbsent {
		err = nil
	} else if device.v2 != nil {
		result, err = cloudunenroll.Remove(cloudCtx, conn, auth, device.v2)
	} else {
		result.CertificatesRevoked, result.AssetDeleted, err = cloudUnenrollCleanupClient(cloudCtx, conn, device.legacy.GetId())
	}
	var resetErr error
	deviceReset := false
	if reset != nil && err == nil {
		resetErr = reset()
		deviceReset = resetErr == nil
	}
	if jsonOutput {
		out := map[string]any{
			"assetId": device.key, "cloud": auth.CloudGRPC, "deviceReset": deviceReset,
			"certsRevoked": result.CertificatesRevoked, "credentialsRevoked": result.CredentialsRevoked,
			"assetDeleted": result.AssetDeleted,
		}
		if err != nil {
			out["cloudError"] = err.Error()
		}
		if reset != nil {
			out["deviceId"] = deviceID
			if hardwareID != "" {
				out["hardwareId"] = hardwareID
			}
			out["assetAlreadyAbsent"] = assetAbsent
		}
		if resetErr != nil {
			out["resetError"] = resetErr.Error()
		}
		if encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(out); encodeErr != nil {
			return encodeErr
		}
	} else if err == nil {
		if assetAbsent {
			fmt.Fprintf(cmd.OutOrStdout(), "No cloud enrollment for %q exists in the selected organization in %s.\n", label, auth.CloudGRPC)
		} else {
			fmt.Fprintf(cmd.OutOrStdout(), "Unenrolled %s: revoked %d certificate(s) and %d enrollment credential(s), and deleted cloud asset %s.\n", device.GetName(), result.CertificatesRevoked, result.CredentialsRevoked, device.key)
		}
		if deviceReset {
			fmt.Fprintln(cmd.OutOrStdout(), "Cleared the board's enrollment identity. WiFi settings and apps were preserved. Run 'wendy device enroll' to enroll again.")
		} else if reset == nil && device.v2 != nil && isLiteCloudAsset(device.v2) {
			fmt.Fprintln(cmd.OutOrStdout(), "The device was not reset. Connect this Lite board over USB and run 'wendy device unenroll' to clear its stored identity before enrolling again.")
		} else if reset == nil {
			fmt.Fprintln(cmd.OutOrStdout(), "The device was not reset. If it still holds an issued identity, clear it through device recovery before enrolling again.")
		}
	}
	if err != nil {
		return fmt.Errorf("cloud unenrollment of asset %s failed after revoking %d certificate(s) and %d enrollment credential(s): %w", device.key, result.CertificatesRevoked, result.CredentialsRevoked, err)
	}
	if resetErr != nil {
		return fmt.Errorf("cloud enrollment is absent in the selected organization in %s, but clearing the board's stored identity failed: %w; keep it connected over USB and retry 'wendy device unenroll'", auth.CloudGRPC, resetErr)
	}
	return nil
}
