package sessionbroker

import (
	"context"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"os"
	"time"
)

// PrepareOperatorAutomatic joins with native neighbour discovery over LAN and BLE.
func PrepareOperatorAutomatic(ctx context.Context, certificate config.CertificateInfo) (*grpcclient.AgentConnection, error) {
	return prepareOperatorAutomatic(ctx, certificate, false)
}

// EnableOperatorAutomatic retains an explicitly enabled router independently of
// ordinary command leases. Passive cold discovery alone may still expire idle.
func EnableOperatorAutomatic(ctx context.Context, certificate config.CertificateInfo) (*grpcclient.AgentConnection, error) {
	return prepareOperatorAutomatic(ctx, certificate, true)
}
func prepareOperatorAutomatic(ctx context.Context, certificate config.CertificateInfo, enabled bool) (*grpcclient.AgentConnection, error) {
	operator, err := operatorPrincipal(certificate)
	if err != nil {
		return nil, err
	}
	// Own discovery/recipe creation across independent processes, not merely
	// startup after selecting a peer. Concurrent cold callers can see different
	// multicast answers; the first recipe wins and the others reuse that owner.
	if !configDirOwnershipSafe() {
		return nil, errors.New("operator profile is not owned by the current user")
	}
	directory, err := configDir()
	if err != nil {
		return nil, err
	}
	brokerDirectory, err := brokerDir()
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDir(brokerDirectory); err != nil {
		return nil, err
	}
	socket, _ := paths(brokerDirectory, operatorKey(operator.Principal()), certs.WendyIdentity{})
	var lock *os.File
	for lock == nil {
		file, acquired, err := acquireIdentityLock(socket + ".discovery")
		if err != nil {
			return nil, err
		}
		if acquired {
			lock = file
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	defer func() { releaseLock(lock); _ = lock.Close() }()
	if recipe, err := readOperatorRecipe(operatorRecipePath(directory, operator.Principal())); err == nil {
		recipe.Enabled = recipe.Enabled || enabled
		return prepareOperatorRecipe(ctx, certificate, recipe)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// The detached broker creates its own signed origin before discovering any
	// first hop. Ordinary bounded LAN/BLE candidates then establish verified TLS.
	return prepareOperatorRecipe(ctx, certificate, OperatorSpec{Version: 2, Principal: operator.Principal(), Enabled: enabled})
}
