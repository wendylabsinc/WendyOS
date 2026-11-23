package commands

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// What to pass instead of a taken name when there is no terminal to ask on.
const (
	noTTYHintInstall = "Pass a different --device-name."
	noTTYHintEnroll  = "Pass a different --name."
	noTTYHintRename  = "Nothing was changed.\n  Choose another name:  wendy device rename <new-name>"
)

// orgNameLookupTimeout bounds the early name check; Cloud's refusal at
// reservation still catches a name the check could not see.
const orgNameLookupTimeout = 10 * time.Second

// nameTakenError is the short refusal of a taken name without a terminal to
// ask on.
type nameTakenError string

func (e nameTakenError) Error() string { return string(e) }

func takenNameMessage(name string) string {
	return name + " is already used by another device in your organization."
}

// suggestFreeName offers the first of name-2 … name-9 that fits maxLen and is
// not taken, or "" when none does.
func suggestFreeName(name string, maxLen int, taken func(string) bool) string {
	for i := 2; i <= 9; i++ {
		candidate := fmt.Sprintf("%s-%d", name, i)
		if len(candidate) <= maxLen && !taken(candidate) {
			return candidate
		}
	}
	return ""
}

var fetchOrgAssetsFn = fetchCloudAssetsV2

// orgDeviceNamesFn lists the names of the organization's compute devices, the
// set in which Cloud keeps names unique.
var orgDeviceNamesFn = func(ctx context.Context, auth *config.AuthConfig) ([]string, error) {
	if auth == nil || len(auth.Certificates) == 0 {
		return nil, errors.New("the Wendy Cloud session has no certificate")
	}
	ctx, cancel := context.WithTimeout(ctx, orgNameLookupTimeout)
	defer cancel()
	assets, err := fetchOrgAssetsFn(ctx, auth, false)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(assets))
	for _, a := range assets {
		if n := a.GetName(); n != "" {
			names = append(names, n)
		}
	}
	return names, nil
}

// newNameChecker matches names case-insensitively, as Cloud does; a failed
// lookup counts every name as free and leaves the decision to the reservation.
func newNameChecker(ctx context.Context, auth *config.AuthConfig) func(string) bool {
	var once sync.Once
	var names []string
	return func(name string) bool {
		once.Do(func() { names, _ = orgDeviceNamesFn(ctx, auth) })
		for _, n := range names {
			if strings.EqualFold(n, name) {
				return true
			}
		}
		return false
	}
}

var promptDeviceNameDefault = func(prompt, hint, def string, validate tui.ValidateFunc) (string, error) {
	return tui.PromptTextWithDefault(prompt, hint, def, validate)
}

// nameRetry is how one command asks again for a name Cloud refused as taken.
type nameRetry struct {
	interactive bool
	noTTYHint   string
	validate    tui.ValidateFunc
	maxLen      int
}

// retryTakenDeviceName retries enroll while Cloud refuses *name as taken,
// asking for another name in a terminal; Ctrl+C returns tui.ErrCancelled.
func retryTakenDeviceName(name *string, r nameRetry, enroll func(string) error) error {
	base := *name
	refused := map[string]bool{}
	err := enroll(*name)
	for errors.Is(err, cloudenroll.ErrDeviceNameTaken) {
		if !r.interactive {
			return nameTakenError(takenNameMessage(*name) + " " + r.noTTYHint)
		}
		refused[strings.ToLower(*name)] = true
		fmt.Fprintln(os.Stdout, tui.ErrorMessage(takenNameMessage(*name)))
		suggestion := suggestFreeName(base, r.maxLen, func(n string) bool { return refused[strings.ToLower(n)] })
		next, perr := promptDeviceNameDefault("Device name", "(Ctrl+C to stop)", suggestion, r.validate)
		if perr != nil {
			return perr
		}
		*name = next
		err = enroll(*name)
	}
	return err
}
