//go:build darwin || linux || windows

package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	linuxDesktopValue    = "linux-desktop"
	headlessMacValue     = "headless-mac"
	linuxDesktopAgentURL = "https://install.wendy.dev/agent.sh"

	// Machine labels woven into the install instructions. The agent.sh command
	// is identical for both — it auto-detects the platform (uname -s) and does
	// the right thing — so only the surrounding prose differs.
	linuxDesktopMachineLabel = "Linux machine"
	headlessMacMachineLabel  = "Mac"
)

// linuxDesktopCmdStyle renders the copyable install command in bold sky (the
// same hue tui.Command uses for copyable commands), so it stands out from the
// surrounding prose as the thing to copy. lipgloss strips styling on non-TTY
// output, so piped/redirected runs stay plain.
var linuxDesktopCmdStyle = lipgloss.NewStyle().Foreground(tui.Sky500).Bold(true)

// renderLinuxDesktopInstructions returns the text printed when the user picks
// "Linux Desktop" or "Headless Mac". machineLabel names the target machine in
// the prose (e.g. "Linux machine" or "Mac"); the agent.sh command itself is
// identical, since the script auto-detects the platform. With an empty token
// it prints the plain (unenrolled) docs command; with a token it prints the
// pre-enrollment one-liner.
func renderLinuxDesktopInstructions(token, cloudHost, orgName, machineLabel string, expiresAt time.Time) string {
	var b strings.Builder
	if token == "" {
		fmt.Fprintf(&b, "%s\n\n", tui.Header("Install wendy-agent on your "+machineLabel+":"))
		fmt.Fprintf(&b, "  %s\n\n", linuxDesktopCmdStyle.Render(linuxDesktopCommand("", "")))
		fmt.Fprintf(&b, "The device is discovered over your local network — run %s.\n", tui.Command("wendy discover"))
		fmt.Fprintf(&b, "To enroll it into an org later, run %s\n", tui.Command("wendy device enroll"))
		fmt.Fprintf(&b, "%s\n", tui.Dim("(or re-run `wendy install` while logged in for a pre-enrollment token)."))
		return b.String()
	}
	fmt.Fprintf(&b, "Install wendy-agent on your %s; it will enroll into %s automatically.\n\n", machineLabel, tui.Device(orgName))
	fmt.Fprintf(&b, "  %s\n\n", linuxDesktopCmdStyle.Render(linuxDesktopCommand(token, cloudHost)))
	fmt.Fprintf(&b, "This enrollment token expires at %s (about 1 hour). Run the command before then.\n", tui.Value(expiresAt.Format(time.RFC1123)))
	fmt.Fprintf(&b, "After it boots, run %s to find the device.\n", tui.Command("wendy discover"))
	return b.String()
}

// linuxDesktopCommand returns the runnable, single-line agent.sh install
// command — the form copied to the clipboard so it pastes and runs directly.
// The multi-line display form lives in renderLinuxDesktopInstructions.
func linuxDesktopCommand(token, cloudHost string) string {
	if token == "" {
		return fmt.Sprintf("curl -fsSL %s | bash", linuxDesktopAgentURL)
	}
	return fmt.Sprintf("curl -fsSL %s | WENDY_ENROLLMENT_TOKEN=%s WENDY_CLOUD_HOST=%s bash",
		linuxDesktopAgentURL, token, cloudHost)
}

// copyLinuxDesktopCommand copies the runnable install command to the clipboard
// and prints a confirmation, returning whether it succeeded. It is a
// best-effort convenience: it no-ops when not attached to an interactive
// terminal, and a clipboard failure (no tool, headless host) is silently
// ignored rather than surfaced as an error.
func copyLinuxDesktopCommand(token, cloudHost string, interactive bool) bool {
	if !interactive {
		return false
	}
	if err := clipboardWriter(linuxDesktopCommand(token, cloudHost)); err != nil {
		return false
	}
	fmt.Fprintln(os.Stdout, "\n"+tui.SuccessMessage("Copied the command to your clipboard."))
	return true
}

// linuxDesktopTokenFn is a stub point so tests can replace token minting
// without dialing Wendy Cloud.
var linuxDesktopTokenFn = createLinuxDesktopToken

// linuxDesktopConfigLoad is a stub point so tests can replace config loading
// without reading ~/.wendy/config.json from disk.
var linuxDesktopConfigLoad = config.Load

// installDesktop prints agent.sh install instructions for turning an
// existing Linux machine or Mac into a managed Wendy device. machineLabel
// names the target in the printed prose ("Linux machine" or "Mac"); the
// agent.sh command is identical for both because the script auto-detects the
// platform. When the user is logged in and does not decline, it mints a
// short-lived enrollment token and prints the pre-enrollment one-liner. It
// never writes a drive or downloads an image.
func installDesktop(ctx context.Context, preOpts preEnrollOptions, deviceName, machineLabel string) error {
	interactive := isInteractiveTerminal()

	cfg, err := linuxDesktopConfigLoad()
	if err != nil {
		cfg = &config.Config{} // treat an unreadable config as "not logged in"
	}

	enroll := false
	switch preOpts.mode {
	case preEnrollSkip:
		enroll = false
	case preEnrollForced:
		enroll = true
	case preEnrollAuto:
		if interactive && len(cfg.Auth) > 0 {
			ok, cErr := confirmPreEnroll()
			if cErr != nil {
				return cErr
			}
			enroll = ok
		}
	}

	var token, cloudHost, orgName string
	var expiresAt time.Time
	if enroll {
		auth, aErr := selectEnrollmentAuth(cfg, preOpts.cloudGRPC, interactive)
		if aErr != nil {
			if errors.Is(aErr, ErrUserCancelled) {
				return aErr
			}
			if !interactive {
				return fmt.Errorf("--pre-enroll: %w", aErr)
			}
			fmt.Fprintf(os.Stdout, "Cannot pre-enroll: %v\n", aErr)
			// fall through to plain instructions
		} else if auth != nil {
			org, oErr := resolveOrg(ctx, auth, false)
			if oErr != nil {
				if errors.Is(oErr, ErrUserCancelled) {
					return oErr
				}
				if !interactive {
					return fmt.Errorf("--pre-enroll: resolving organization: %w", oErr)
				}
				fmt.Fprintf(os.Stdout, "Cannot resolve organization: %v\n", oErr)
			} else {
				provName, nErr := resolveDeviceName(deviceName)
				if nErr != nil {
					return nErr
				}
				tok, exp, tErr := linuxDesktopTokenFn(ctx, auth, provName, org.ID)
				if tErr != nil {
					if !interactive {
						return fmt.Errorf("--pre-enroll: creating enrollment token: %w", tErr)
					}
					fmt.Fprintf(os.Stdout, "Could not create enrollment token: %v\n", tErr)
				} else {
					token, cloudHost, orgName, expiresAt = tok, auth.CloudGRPC, org.Name, exp
				}
			}
		}
	}

	fmt.Fprint(os.Stdout, renderLinuxDesktopInstructions(token, cloudHost, orgName, machineLabel, expiresAt))
	copyLinuxDesktopCommand(token, cloudHost, interactive)
	return nil
}

// createLinuxDesktopToken mints a short-lived asset enrollment token for org.
// The CLI does NOT issue a certificate — only the token is handed to the
// device, which self-enrolls. Mirrors the cloud dial in preEnrollDevice.
func createLinuxDesktopToken(ctx context.Context, auth *config.AuthConfig, deviceName string, orgID int32) (string, time.Time, error) {
	if len(auth.Certificates) == 0 {
		return "", time.Time{}, fmt.Errorf("auth session has no certificates; re-run 'wendy auth login'")
	}
	cert := auth.Certificates[0]

	var transportOpt grpc.DialOption
	if strings.HasSuffix(auth.CloudGRPC, ":443") {
		keyPEM, err := cert.PrivateKeyPEM()
		if err != nil {
			return "", time.Time{}, fmt.Errorf("loading client key: %w", err)
		}
		tlsCfg, err := certs.LoadTLSConfig(cert.PemCertificate, cert.PemCertificateChain, keyPEM, "")
		if err != nil {
			return "", time.Time{}, fmt.Errorf("loading TLS config: %w", err)
		}
		transportOpt = grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))
	} else {
		transportOpt = grpc.WithTransportCredentials(insecure.NewCredentials())
	}

	dialOptions, err := withCloudRequestSigning(auth, transportOpt)
	if err != nil {
		return "", time.Time{}, err
	}
	conn, err := grpc.NewClient(auth.CloudGRPC, dialOptions...)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("connecting to cloud: %w", err)
	}
	defer conn.Close()

	if orgID == 0 {
		orgID = int32(cert.OrganizationID)
	}
	cloudCtx, err := cloudContext(ctx, auth)
	if err != nil {
		return "", time.Time{}, err
	}
	resp, err := cloudpb.NewCertificateServiceClient(conn).CreateAssetEnrollmentToken(cloudCtx, &cloudpb.CreateAssetEnrollmentTokenRequest{
		OrganizationId: orgID,
		Name:           deviceName,
		TtlSeconds:     3600,
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("creating enrollment token: %w", err)
	}
	var expiresAt time.Time
	if resp.GetExpiresAt() != nil {
		expiresAt = resp.GetExpiresAt().AsTime()
	}
	return resp.GetEnrollmentToken(), expiresAt, nil
}
