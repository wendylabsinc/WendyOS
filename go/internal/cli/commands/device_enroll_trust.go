package commands

import (
	"context"
	"errors"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

var confirmEnrollmentTrustFn = func() (bool, error) {
	return tui.ConfirmDefaultNo("Do you want to trust this device anyways?", tea.WithOutput(os.Stderr))
}

// runWithEnrollmentTarget keeps a trust override inside enrollment. It never
// unpins an offline device or retries selection against a different device.
func runWithEnrollmentTarget(ctx context.Context, connect func() (*SelectedDevice, error), enroll func(*SelectedDevice) error) error {
	target, err := connect()
	var trust *enrollmentTrust
	if err != nil {
		target, trust, err = recoverEnrollmentTrust(ctx, err)
		if err != nil {
			return err
		}
	}
	defer target.Close()
	var saveBindings []func() error
	target.recordEnrollmentBinding = func(save func() error) { saveBindings = append(saveBindings, save) }
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := enroll(target); err != nil {
		return err
	}
	if trust != nil {
		if err := trust.clearPreviousIdentity(); err != nil {
			return err
		}
	}
	for _, save := range saveBindings {
		if err := save(); err != nil {
			return err
		}
	}
	return nil
}

type enrollmentTrust struct {
	pinKey    string
	pinnedKey string
	pin       config.DevicePin
}

// connectUnboundEnrollmentTarget is used only by explicit enrollment, before
// the ordinary management dial. It never overrides a saved identity, including
// an older incomplete pin, and never treats an enrolled or unknown state as a
// setup endpoint. The connection stays scoped to the enrollment callback.
func connectUnboundEnrollmentTarget(ctx context.Context, pinKey, addr string) (*SelectedDevice, bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, true, err
	}
	if _, _, pinned := lookupPin(cfg, pinKey); pinned {
		return nil, false, nil
	}
	conn, err := connectEnrollmentPlaintext(ctx, resolveAddrCandidates(ctx, addr))
	if err != nil {
		return nil, true, err
	}
	probeCtx, cancel := context.WithTimeout(ctx, agentPlaintextProbeTimeout)
	defer cancel()
	state, err := deviceProvisioning(probeCtx, conn)
	if err != nil || state.GetNotProvisioned() == nil {
		conn.Close()
		if err == nil {
			err = fmt.Errorf("explicit setup requires an unprovisioned device; enrolled-device management requires a trusted expected identity")
		}
		return nil, true, err
	}
	return &SelectedDevice{Agent: conn}, true, nil
}

func recoverEnrollmentTrust(ctx context.Context, cause error) (*SelectedDevice, *enrollmentTrust, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var failure *noAuthenticatedEndpointError
	if jsonOutput || !isInteractiveTerminal() || !errors.As(cause, &failure) ||
		failure.certSeen || failure.pinKey == "" || failure.pinnedKey == "" || len(failure.candidates) == 0 {
		return nil, nil, cause
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, fmt.Errorf("loading saved device identity: %w", err)
	}
	pin, key, ok := lookupPin(cfg, failure.pinKey)
	if !ok || key != failure.pinnedKey {
		return nil, nil, fmt.Errorf("saved identity for %q changed; retry enrollment", failure.pinKey)
	}
	trust := &enrollmentTrust{pinKey: failure.pinKey, pinnedKey: key, pin: pin}
	fmt.Fprintf(os.Stderr, "Device %q did not answer with an authenticated identity.\n", failure.pinKey)
	fmt.Fprintln(os.Stderr, "Trusting it allows enrollment over an unauthenticated connection. Its previous saved identity will be cleared only after enrollment succeeds.")
	accepted, err := confirmEnrollmentTrustFn()
	if errors.Is(err, tui.ErrCancelled) {
		return nil, nil, ErrUserCancelled
	}
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if !accepted {
		return nil, nil, cause
	}

	// The failed authenticated dial captured these addresses. Do not reopen
	// the picker or consult a newly selected default after consent.
	conn, err := connectEnrollmentPlaintext(ctx, failure.candidates)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to trusted device %q for enrollment: %w", failure.pinKey, err)
	}
	// This connection is authorized only for the enrollment callback. Giving
	// it a PinKey would reapply the old identity before enrollment can replace it.
	return &SelectedDevice{Agent: conn}, trust, nil
}

func connectEnrollmentPlaintext(ctx context.Context, candidates []string) (*grpcclient.AgentConnection, error) {
	var failures []error
	for _, addr := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		conn, err := plaintextConnectFn(ctx, addr)
		if err == nil {
			// Connect is lazy. Require a live agent before sending enrollment
			// material, and give each candidate a bounded probe.
			probeCtx, cancel := context.WithTimeout(ctx, agentPlaintextProbeTimeout)
			version, probeErr := conn.AgentService.GetAgentVersion(probeCtx, &agentpb.GetAgentVersionRequest{})
			cancel()
			if probeErr == nil {
				conn.CacheAgentVersion(version)
				return conn, nil
			}
			conn.Close()
			err = probeErr
		}
		failures = append(failures, fmt.Errorf("%s: %w", addr, err))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return nil, errors.Join(failures...)
}

func (t *enrollmentTrust) clearPreviousIdentity() error {
	var cleared []clearedPin
	err := config.Update(func(cfg *config.Config) (bool, error) {
		pin, key, ok := lookupPin(cfg, t.pinKey)
		if !ok || key != t.pinnedKey || pin != t.pin {
			return false, fmt.Errorf("saved identity for %q changed during enrollment", t.pinKey)
		}
		cleared = clearPinsGoverning(cfg, t.pinKey)
		return clearedAnyConfigPin(cleared), nil
	})
	if err != nil {
		return fmt.Errorf("device enrolled, but its previous saved identity could not be cleared: %w", err)
	}
	_ = sessionbroker.InvalidateAll()
	printClearedPins(os.Stderr, cleared)
	return nil
}
