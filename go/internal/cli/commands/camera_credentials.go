package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"github.com/wendylabsinc/wendy/go/internal/shared/streamreason"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
)

// Credentials for a network camera are resolved in two steps, and only ever for a
// camera that actually asked for them:
//
//  1. wendy.json, so an unattended deploy needs no prompt.
//  2. an interactive prompt, so a developer with no wendy.json is not stuck.
//
// Local cameras never reach here: nothing asks a USB webcam to log in. The
// trigger is the agent reporting IP_CAMERA_NO_CREDENTIALS, so the flow is driven
// by the camera's own requirement rather than by guessing from the transport.

// cameraCredentialsFromConfig reads camera credentials out of wendy.json in dir.
// Missing file, missing entitlement and missing fields are all "not configured"
// rather than errors: this is an optional convenience.
func cameraCredentialsFromConfig(dir string) (user, password string, found bool) {
	cfg, err := appconfig.LoadFromFile(filepath.Join(dir, "wendy.json"))
	if err != nil {
		return "", "", false
	}
	for _, ent := range cfg.Entitlements {
		if ent.Type != appconfig.EntitlementCamera && ent.Type != appconfig.EntitlementVideo {
			continue
		}
		if ent.User == "" {
			continue
		}
		return ent.User, ent.Password, true
	}
	return "", "", false
}

// cameraNeedsCredentials reports the device ID a failed request wants a login for.
// It is the agent's machine-readable signal, so a stream that failed for any other
// reason is left alone.
func cameraNeedsCredentials(err error) (uint32, bool) {
	info := streamreason.Info(err)
	if info == nil || info.GetReason() != streamreason.IPCameraNoCredentials {
		return 0, false
	}
	id, parseErr := parseCameraID(info.GetMetadata()["device_id"])
	if parseErr != nil {
		return 0, false
	}
	return id, true
}

// streamVideoWithCredentialRetry starts a camera stream and resolves a missing
// network-camera login exactly once, whether gRPC reports it while constructing
// the server stream or on the first Recv call. grpc-go normally uses the latter
// path for server-handler status errors.
func streamVideoWithCredentialRetry(
	start func() (videoStream, error),
	resolve func(deviceID uint32) error,
) (videoStream, error) {
	stream, err := start()
	attempted := false
	if deviceID, ok := cameraNeedsCredentials(err); ok {
		attempted = true
		if err := resolve(deviceID); err != nil {
			return nil, err
		}
		stream, err = start()
	}
	if err != nil {
		return nil, err
	}
	return &cameraCredentialRetryStream{
		stream:    stream,
		start:     start,
		resolve:   resolve,
		attempted: attempted,
	}, nil
}

// cameraCredentialRetryStream intercepts the first receive-side credentials
// error. Once a retry has been attempted, every result passes through unchanged
// so a bad login cannot trigger an infinite prompt loop.
type cameraCredentialRetryStream struct {
	stream    videoStream
	start     func() (videoStream, error)
	resolve   func(deviceID uint32) error
	attempted bool
}

func (s *cameraCredentialRetryStream) Recv() (*agentpb.VideoFrame, error) {
	frame, err := s.stream.Recv()
	if s.attempted {
		return frame, err
	}
	deviceID, ok := cameraNeedsCredentials(err)
	if !ok {
		return frame, err
	}

	s.attempted = true
	if err := s.resolve(deviceID); err != nil {
		return nil, err
	}
	next, err := s.start()
	if err != nil {
		return nil, err
	}
	s.stream = next
	return s.stream.Recv()
}

// errNoCameraCredentials is returned when neither wendy.json nor a terminal can
// supply a login.
var errNoCameraCredentials = errors.New("no camera credentials available")

// resolveCameraCredentials supplies a login for deviceID, from wendy.json if it
// has one and from a prompt otherwise, and stores it on the device.
//
// promptAllowed is false for non-interactive runs, where prompting would hang.
func resolveCameraCredentials(
	ctx context.Context,
	cmd *cobra.Command,
	set func(context.Context, *agentpb.SetCameraCredentialsRequest) error,
	deviceID uint32,
	dir string,
	promptAllowed bool,
) error {
	user, password, found := cameraCredentialsFromConfig(dir)
	switch {
	case found:
		cliLogln("Using camera credentials from wendy.json for camera %d.", deviceID)
	case promptAllowed:
		user = defaultCameraUser
		cliLogln("Camera %d needs a login.", deviceID)
		secret, err := readCameraPassword(cmd, deviceID)
		if err != nil {
			return err
		}
		password = secret
	default:
		return fmt.Errorf(
			"%w: add a camera entitlement with user and password to wendy.json, "+
				"set WENDY_CAMERA_PASSWORD, or run `wendy device camera login %d`",
			errNoCameraCredentials, deviceID)
	}

	if err := set(ctx, &agentpb.SetCameraCredentialsRequest{
		DeviceId: deviceID,
		Username: user,
		Password: password,
	}); err != nil {
		return fmt.Errorf("storing camera credentials: %w", err)
	}
	return nil
}

// defaultCameraUser is the username assumed when only a password is supplied.
// Every IP camera vendor ships "admin" as the administrative account.
const defaultCameraUser = "admin"

// cameraPromptAllowed reports whether a password can be obtained without
// blocking. An environment-supplied password counts, since it needs no prompt.
func cameraPromptAllowed(nonInteractive bool) bool {
	if os.Getenv("WENDY_CAMERA_PASSWORD") != "" {
		return true
	}
	return !nonInteractive && term.IsTerminal(int(os.Stdin.Fd()))
}
