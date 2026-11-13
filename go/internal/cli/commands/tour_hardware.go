//go:build darwin || linux || windows

package commands

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/analytics"
)

func tourNeedsNativeInstaller(d *deviceInfo) bool {
	return d != nil && (d.Key == thorDeviceType || isT234RecoveryDevice(d.Key))
}

func tourNativeInstallArgs(d *deviceInfo) []string {
	args := []string{"install", "--device-type", d.Key}
	if d.LatestVersion != "" {
		args = append(args, "--version", d.LatestVersion)
	}
	// Legacy images have no recovery bundle; let the existing installer explain
	// and choose its supported storage flow rather than inventing a plan here.
	if isT234RecoveryDevice(d.Key) && d.Manifest != nil && d.Manifest.Versions[d.LatestVersion].InstallMode == "recovery" {
		args = append(args, "--rootfs-only=false")
	}
	return args
}

func (m tourWizardModel) cmdNativeOSInstall() tea.Cmd {
	exe, err := os.Executable()
	if err != nil {
		return func() tea.Msg { return tourOSInstallDoneMsg{err: err, native: true} }
	}
	cmd := exec.CommandContext(m.ctx, exe, tourNativeInstallArgs(m.selected)...)
	cmd.Env = append(os.Environ(), "WENDY_TELEMETRY_ATTEMPT_ID="+analytics.AttemptID(m.ctx), "WENDY_TELEMETRY_ENTRYPOINT=tour")
	return tea.ExecProcess(cmd, func(err error) tea.Msg { return tourOSInstallDoneMsg{err: tourChildError(m.ctx, err), native: true} })
}

// A cancelled tour child uses exit 130 so declining the installer's own
// destructive-write confirmation cannot be mistaken for a completed install.
func tourChildError(ctx context.Context, err error) error {
	if ctx != nil && errors.Is(ctx.Err(), context.Canceled) {
		return ErrUserCancelled
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 130 {
		return ErrUserCancelled
	}
	return err
}

// Suspend Bubble Tea while the Linux USB preflight may ask for sudo. Running
// the prompt inside an asynchronous model command would paint over the input.
type tourTerminalAction struct{ run func() error }

func (a tourTerminalAction) Run() error        { return a.run() }
func (tourTerminalAction) SetStdin(io.Reader)  {}
func (tourTerminalAction) SetStdout(io.Writer) {}
func (tourTerminalAction) SetStderr(io.Writer) {}

func (m tourWizardModel) cmdHardwareDiscoveryStart() tea.Cmd {
	return tea.Exec(tourTerminalAction{run: func() error { return maybeOfferUSBSetup(m.ctx) }}, func(err error) tea.Msg {
		if err != nil {
			return tourDiscoveryErrorMsg{err: err}
		}
		return tourDiscoveryTickMsg{}
	})
}

func (m tourWizardModel) sampleURL() string {
	host := strings.TrimSpace(m.foundAddr)
	if host == "" {
		host = m.hostname
	}
	if host == "" {
		host = m.targetName + ".local"
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return "http://" + net.JoinHostPort(host, "8000")
}

const tourSampleResponseLimit = 64 << 10

// This HTML marker is emitted by the built-in tourAppPy starter. A different
// service already listening on port 8000 must not satisfy the tour's readiness.
const tourSampleResponseMarker = "<p>Your Wendy edge device is running.</p>"

func tourSampleResponseReady(statusCode int, body io.Reader) bool {
	if statusCode != http.StatusOK {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(body, tourSampleResponseLimit+1))
	return err == nil && len(data) <= tourSampleResponseLimit && bytes.Contains(data, []byte(tourSampleResponseMarker))
}

func (m tourWizardModel) cmdSampleReadiness() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(m.ctx, 45*time.Second)
		defer cancel()
		url := m.sampleURL()
		client := &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{Proxy: nil}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		defer client.CloseIdleConnections()
		for {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return tourSampleReadyMsg{err: err}
			}
			resp, err := client.Do(req)
			if err == nil {
				ready := tourSampleResponseReady(resp.StatusCode, resp.Body)
				resp.Body.Close()
				if ready {
					tourStage(ctx, "physical", "app_ready", "completed", nil, "readiness")
					_ = browserOpen(url)
					return tourSampleReadyMsg{}
				}
			}
			select {
			case <-ctx.Done():
				if errors.Is(ctx.Err(), context.Canceled) {
					return tourSampleReadyMsg{err: ErrUserCancelled}
				}
				return tourSampleReadyMsg{err: fmt.Errorf("the app was deployed, but %s did not return the tour starter response before the readiness deadline; inspect 'wendy device logs --device %s' and rerun from %s: %w", url, m.foundAddr, m.projectPath, ctx.Err())}
			case <-time.After(time.Second):
			}
		}
	}
}
