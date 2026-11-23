package commands

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
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

// localKeyLabel is "<hostname>/<user>", so an owner can tell whose key it is.
func localKeyLabel() string {
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
