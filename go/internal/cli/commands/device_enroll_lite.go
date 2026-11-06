package commands

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/liteenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/providers"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/protobuf/proto"
)

type liteEnrollmentOptions struct {
	csrURL, timeURL, brokerHost       string
	caCertsURL                        string
	deviceRoots, tsaRoots, httpsRoots string
	brokerPort                        uint32
}

type liteEnrollmentDevice interface {
	GetDeviceIdentity(time.Duration) (*liteclient.DeviceIdentity, error)
	EnrollmentChallenge(bool) (*litepb.WendyComEnrollmentChallenge, error)
	SyncTimeChallenge(context.Context, string) (time.Time, error)
	PushConf(*litepb.WendyConf, liteclient.ConfPushMode, func(uint32, uint32)) error
	ResetTargetDevice(bool, time.Duration) error
	WaitReboot(context.Context)
	Close() error
}

// Enrollment uses the selected device's protocol, sharing auth and selection
// with the existing enroll command and the provider's connection handling.
func runSelectedDeviceEnrollment(cmd *cobra.Command, target *SelectedDevice, auth *config.AuthConfig, name string, orgID int32, acmeDirectoryURL string, opts liteEnrollmentOptions) error {
	if target.External != nil && target.External.ProviderKey == "wendy-lite" {
		if orgID != 0 || acmeDirectoryURL != "" {
			return fmt.Errorf("Wendy Lite uses the selected session's tenant and CSR enrollment; --org and --acme-directory-url do not apply")
		}
		return runLiteEnrollmentFn(cmd, *target.External, auth, name, opts)
	}
	if opts.deviceRoots != "" || opts.tsaRoots != "" || opts.httpsRoots != "" || opts.caCertsURL != "" {
		return fmt.Errorf("trust bundle flags apply only to Wendy Lite devices")
	}
	conn, err := connectFromSelectedDevice(target, resolveConfig{suppressProvisioningHint: true})
	if err != nil {
		return err
	}
	if err := promptEnrollmentWifiFn(cmd.Context(), conn); err != nil {
		return err
	}
	return runAgentEnrollmentFn(cmd.Context(), conn, auth, name, orgID, acmeDirectoryURL)
}

var (
	errLiteUUIDUnsupported  = errors.New("this Wendy Lite firmware cannot enroll with a device UUID; update the firmware over USB before enrolling")
	errLiteAlreadyEnrolled  = errors.New("this board still holds an issued identity, even if its cloud asset was removed; connect it over USB and run 'wendy device unenroll' with that USB device selected, then enroll again; older firmware must be updated first")
	connectLiteEnrollmentFn = func(target models.ExternalDevice) (liteEnrollmentDevice, error) {
		return (&providers.MicroWendyProvider{}).ConnectEnrollment(target)
	}
	runLiteEnrollmentFn    = runEnrollLiteDevice
	runAgentEnrollmentFn   = runEnrollDevice
	promptEnrollmentWifiFn = promptWifiIfNeeded
)

func runEnrollLiteDevice(cmd *cobra.Command, target models.ExternalDevice, auth *config.AuthConfig, name string, opts liteEnrollmentOptions) error {
	if target.ConnectionType() != "USB" {
		return fmt.Errorf("Wendy Lite enrollment requires a USB connection; connect the board over USB and select its USB device with --device")
	}
	ctx := cmd.Context()
	device, err := connectLiteEnrollmentFn(target)
	if err != nil {
		return err
	}
	defer device.Close()
	identity, err := device.GetDeviceIdentity(5 * time.Second)
	if err != nil {
		return err
	}
	challenge, err := device.EnrollmentChallenge(true)
	if err != nil {
		return err
	}
	if err := checkLiteEnrollmentState(challenge); err != nil {
		return err
	}
	trustProfile := liteenroll.TrustProfileFromChallenge(challenge)
	if opts.deviceRoots != "" {
		trustProfile = trustProfile.WithExistingDeviceIssuer()
	}
	// Older firmware does not report this capability. Reject only firmware that
	// explicitly reports its certificate-issuance task is unavailable.
	if challenge.CloudEnrollmentSupported != nil && !challenge.GetCloudEnrollmentSupported() {
		return fmt.Errorf("this Wendy Lite firmware was built without cloud enrollment support; rebuild it with PKI, cloud and Wi-Fi support enabled before enrolling")
	}
	// The device's one id is a UUID the CLI mints. Firmware that cannot store
	// a UUID identity is refused, never enrolled under its hardware id.
	if !challenge.GetUuidEnrollmentSupported() {
		return errLiteUUIDUnsupported
	}
	name, err = enrollmentName(identity.Name, name)
	if err != nil {
		return err
	}
	cfg, err := liteenroll.Config(auth, uuid.NewString(), opts.csrURL, opts.timeURL, opts.brokerHost, opts.brokerPort)
	if err != nil {
		return err
	}
	if opts.caCertsURL != "" && (opts.deviceRoots != "" || opts.tsaRoots != "" || opts.httpsRoots != "") {
		return fmt.Errorf("use either --ca-certs-url or explicit root bundle files")
	}
	timeClient, err := liteenroll.ProvisionTrustForProfile(cfg, opts.deviceRoots, opts.tsaRoots, opts.httpsRoots, trustProfile)
	if err != nil {
		return err
	}
	defer timeClient.CloseIdleConnections()
	challenge, err = device.EnrollmentChallenge(false)
	if err != nil {
		return err
	}
	if err := checkLiteEnrollmentState(challenge); err != nil {
		return err
	}
	// The default TSA's pinned root and verified HTTPS anchor must reach the
	// board even when it has built-in roots for another timestamp authority.
	if cfg.TimeUrl == liteenroll.DefaultTimeURL || (!cfg.ProvisionTrust && (!challenge.GetBuiltinRootsReady() || opts.caCertsURL != "")) {
		if !challenge.GetUsbTrustSupported() {
			return fmt.Errorf("firmware does not support trust bundle provisioning; update the board first")
		}
		timeClient, err = liteenroll.DiscoverTrustForProfile(ctx, cfg, opts.caCertsURL, trustProfile)
		if err != nil {
			return err
		}
		defer timeClient.CloseIdleConnections()
	}
	if err := liteenroll.CheckTrustSupport(cfg, challenge); err != nil {
		return err
	}
	if cfg.TimeUrl == "roughtime" {
		if !challenge.GetRoughtimeSupported() {
			return fmt.Errorf("firmware does not support Roughtime; update the board first")
		}
		fmt.Fprintln(cmd.OutOrStdout(), "Synchronizing the board using verified Roughtime replies...")
		if _, err := device.SyncTimeChallenge(ctx, challenge.NonceHex); err != nil {
			return err
		}
	} else {
		cfg.SignedTime, err = liteenroll.SignedTime(ctx, timeClient, cfg.TimeUrl, challenge.NonceHex)
		if err != nil {
			return err
		}
	}
	if len(cfg.SignedTime)+len(cfg.DeviceRoots)+len(cfg.TsaRoots)+len(cfg.HttpsRoots) > 65536 {
		return fmt.Errorf("signed time and trust bundles exceed the firmware's 64 KiB enrollment limit")
	}
	cloud, err := dialCloudGRPC(auth)
	if err != nil {
		return err
	}
	defer cloud.Close()
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return err
	}
	mintCtx, cancel := context.WithTimeout(cloudCtx, 30*time.Second)
	hardware := &cloudpb.DeviceHardware{}
	if identity.ID != "" {
		hardware.SerialNumber = proto.String(identity.ID)
	}
	if target.OSVersion != "" {
		hardware.OsVersion = proto.String(target.OSVersion)
	}
	if target.CPUArchitecture != "" {
		hardware.Architecture = proto.String(target.CPUArchitecture)
	}
	asset, err := liteenroll.Mint(mintCtx, cloud, auth, cfg, name, hardware, trustProfile)
	cancel()
	if err != nil {
		if asset != "" {
			return fmt.Errorf("asset %s was reserved but enrollment could not continue: %w", asset, err)
		}
		return err
	}
	defer func() { cfg.Token = "" }()
	fmt.Fprintf(cmd.OutOrStdout(), "Cloud reserved asset %s for %s. Delivering its enrollment credential over %s.\n", asset, cfg.DeviceId, target.ConnectionType())
	if err = device.PushConf(&litepb.WendyConf{DeviceName: proto.String(name), Enrollment: cfg}, liteclient.ConfPushModeUpdate, nil); err != nil {
		return fmt.Errorf("asset %s was reserved but device setup failed: %w", asset, err)
	}
	if err = device.ResetTargetDevice(true, 0); err != nil {
		return fmt.Errorf("configuration stored; reboot the board to enroll: %w", err)
	}
	device.WaitReboot(ctx)
	device.Close()
	fmt.Fprintln(cmd.OutOrStdout(), "Waiting for the board to obtain and verify its certificate from pki-core...")
	if err = waitLiteEnrollment(ctx, target); err != nil {
		return fmt.Errorf("asset %s is reserved, but enrollment is not confirmed: %w", asset, err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Enrolled %s as asset %s. Broker presence is reported after the board establishes mTLS; check 'wendy cloud discover'.\n", name, asset)
	return nil
}

// A pending enrollment still owns a Cloud asset. Replacing its configuration
// with a new UUID would lose the board's only binding to that reserved asset.
func checkLiteEnrollmentState(challenge *litepb.WendyComEnrollmentChallenge) error {
	if challenge == nil {
		return fmt.Errorf("Wendy Lite returned no enrollment status")
	}
	if challenge.Enrolled {
		return errLiteAlreadyEnrolled
	}
	if challenge.GetDeviceId() != "" || challenge.GetTenantId() != "" {
		return fmt.Errorf("this board still holds a pending enrollment identity; allow enrollment to finish, or connect it over USB and run 'wendy device unenroll' with that USB device selected before enrolling again")
	}
	return nil
}

func waitLiteEnrollment(ctx context.Context, target models.ExternalDevice) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	var last error
	for {
		select {
		case <-ctx.Done():
			if last != nil {
				return fmt.Errorf("%w; last connection error: %v", ctx.Err(), last)
			}
			return fmt.Errorf("%w; check the board's Wi-Fi and PKI logs", ctx.Err())
		case <-ticker.C:
			client, err := connectLiteEnrollmentFn(target)
			if err != nil {
				last = err
				continue
			}
			state, err := client.EnrollmentChallenge(true)
			client.Close()
			if err != nil {
				last = err
				continue
			}
			last = nil
			if state.Enrolled {
				return nil
			}
		}
	}
}
