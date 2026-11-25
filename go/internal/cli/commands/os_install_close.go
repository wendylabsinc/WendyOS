package commands

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ownerKeysSeedFile is the config-partition file the agent installs at first
// start as its access file (agent configpartition.applyTrustedKeysSeed).
const ownerKeysSeedFile = trustedkeys.FileName

// localKeyPath is a variable so tests do not touch the real config directory.
var localKeyPath = localkey.Path

// buildOwnerKeysSeed is `wendy os install --close`: the access file that makes
// the device start Closed with this computer's local key as its only owner.
func buildOwnerKeysSeed(out io.Writer) ([]byte, error) {
	path, err := localKeyPath()
	if err != nil {
		return nil, err
	}
	key, err := localkey.LoadOrCreate(path)
	if err != nil {
		return nil, fmt.Errorf("--close needs this computer's local key: %w", err)
	}
	spki, err := localkey.PublicKeyDER(key)
	if err != nil {
		return nil, err
	}
	seed, err := trustedkeys.Seed(spki, localKeyLabel(), time.Now())
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "The device will start Closed; its owner is this computer's local key %s.\n", trustedkeys.Fingerprint(spki))
	return seed, nil
}

// refuseClose rejects --close on an install type that writes no config
// partition, rather than leaving the device Open without saying so.
func refuseClose(preOpts preEnrollOptions, installType string) error {
	if len(preOpts.ownerKeys) > 0 {
		return usageErrorf("--close is not supported for %s installs: they write no config partition; install without --close, then run 'wendy device access close'", installType)
	}
	return nil
}

// errCloseOnDrive refuses --close where this computer cannot watch the first
// boot: an agent that predates access shapes ignores the seed and the device
// would come up Open without anyone noticing.
var errCloseOnDrive = usageErrorf("--close is not supported when writing a drive: the device boots elsewhere, so this computer cannot check that it came up Closed; install without --close, then run 'wendy device access close' after first boot")

// closeBootWait bounds the wait for first boot after a --close install.
var closeBootWait = installDriverBootWait

// verifyClosedAfterBoot is the check that --close took. An agent that
// predates access shapes ignores the seed and boots Open, so the install
// waits for first boot and fails unless the device reports Closed with this
// computer's key as its owner. Open is not final at once: the image's agent
// may still be updating to the one the install placed beside it.
func verifyClosedAfterBoot(ctx context.Context, name string) error {
	host := installedDeviceHostname(name)
	ctx, cancel := context.WithTimeout(ctx, closeBootWait)
	defer cancel()
	cliLogln("Waiting for %s to boot to check that it started Closed. Keep it connected to this computer.", host)
	var last string
	for {
		seen, ok, refused := closedAccessState(ctx, host)
		if refused != nil {
			return fmt.Errorf("OS written, but this computer refuses to connect to %s: %w", host, refused)
		}
		if ok {
			cliLogln("%s started Closed; this computer's local key is its owner.", host)
			return nil
		} else if seen != "" && seen != last {
			cliLogln("%s is up but %s; waiting in case its agent is still updating.", host, seen)
			last = seen
		}
		select {
		case <-ctx.Done():
			if last == "" {
				last = "was not reached"
			}
			return fmt.Errorf("OS written, but %s did not come up Closed after first boot (it %s), so it may be Open to anyone on its network; check with 'wendy device access --device %s.local', update its agent with 'wendy device update' if it is old, then run 'wendy device access close'", host, last, host)
		case <-time.After(2 * time.Second):
		}
	}
}

// closedAccessState asks host for its access shape. ok means Closed with this
// computer's key as owner; otherwise seen says what was found ("" when the
// device did not answer). refused is an identity refusal (a pin this
// computer holds for the name), which waiting does not change.
func closedAccessState(ctx context.Context, host string) (seen string, ok bool, refused error) {
	conn, err := connectInstallDriverDevice(ctx, host)
	if errors.Is(err, errDeviceIdentityRefused) {
		return "", false, err
	}
	if err != nil {
		return "", false, nil
	}
	defer conn.Close()
	resp, err := agentpbv2.NewWendyAccessServiceClient(conn.Conn).GetAccess(ctx, &agentpbv2.GetAccessRequest{})
	switch {
	case status.Code(err) == codes.Unimplemented:
		return "Open: its agent does not support access shapes and ignored --close", false, nil
	case err != nil:
		return "", false, nil
	case resp.GetShape() != agentpbv2.AccessShape_ACCESS_SHAPE_CLOSED:
		return accessShapeNames[resp.GetShape()] + ", not Closed", false, nil
	case resp.GetCallerRole() != agentpbv2.TrustedKeyRole_TRUSTED_KEY_ROLE_OWNER:
		return "Closed, but this computer's key is not its owner", false, nil
	}
	return "", true, nil
}

// localKeyLabel is "<hostname>/<user>", so an owner can tell whose key it is,
// within the agent's label limit.
func localKeyLabel() string {
	return capKeyLabel(hostUserLabel())
}

// capKeyLabel drops control characters and cuts s to trustedkeys.MaxLabelLen bytes on
// a character boundary.
func capKeyLabel(s string) string {
	s = tui.StripControl(s)
	if len(s) > trustedkeys.MaxLabelLen {
		s = strings.ToValidUTF8(s[:trustedkeys.MaxLabelLen], "")
	}
	return s
}

func hostUserLabel() string {
	host, _ := os.Hostname()
	if host == "" {
		host = "unknown"
	}
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	if s := os.Getenv("SUDO_USER"); s != "" && os.Geteuid() == 0 {
		name = s // an elevated install runs for the user who ran sudo
	}
	return host + "/" + name
}
