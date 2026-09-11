package llbgen

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/moby/buildkit/client/llb"
	"github.com/opencontainers/go-digest"
)

// digestOf converts a recipe's "sha256:<hex>" checksum into the digest type
// llb.Checksum takes. ir.Lower normalizes every checksum into that spelling,
// so a malformed one here means a hand-built graph — llb.HTTP would stash the
// parse failure and surface it from Marshal with no indication of which
// download was at fault, so it is validated where the URL is still in hand.
func digestOf(checksum string) digest.Digest {
	return digest.Digest(checksum)
}

// parseFileMode converts a Stagefile mode ("0755") into the file mode
// BuildKit's copy and HTTP options take.
//
// Base 8 is forced rather than inferred: strconv would read an unprefixed
// "755" as decimal 755, which is 0o1363 — a mode nobody wrote and the
// Dockerfile backend never produces, since it passes the string through to
// --chmod for the daemon to parse as octal.
func parseFileMode(mode string) (os.FileMode, error) {
	v, err := strconv.ParseUint(strings.TrimPrefix(mode, "0o"), 8, 32)
	if err != nil {
		return 0, fmt.Errorf("mode %q is not octal: %w", mode, err)
	}
	return os.FileMode(v), nil
}

// parseChown converts a Stagefile owner ("1000", "1000:1000", "app:app") into
// BuildKit's ChownOpt.
//
// Numeric ids become UIDs and GIDs directly; anything else is passed through
// as a name for the daemon to resolve against the image's /etc/passwd, which
// is what the Dockerfile backend's --chown does. An omitted group means "the
// user's own group", so it is left unset rather than defaulted to the uid —
// defaulting would silently differ from --chown=1000, which resolves the
// group from the image.
func parseChown(owner string) (*llb.ChownOpt, error) {
	if owner == "" {
		return nil, fmt.Errorf("owner is empty")
	}
	user, group, hasGroup := strings.Cut(owner, ":")
	if user == "" || (hasGroup && group == "") {
		return nil, fmt.Errorf("owner %q must be user, user:group, or :group", owner)
	}
	opt := &llb.ChownOpt{User: userOpt(user)}
	if hasGroup {
		opt.Group = userOpt(group)
	}
	return opt, nil
}

func userOpt(s string) *llb.UserOpt {
	if id, err := strconv.Atoi(s); err == nil {
		return &llb.UserOpt{UID: id}
	}
	return &llb.UserOpt{Name: s}
}
