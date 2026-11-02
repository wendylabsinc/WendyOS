package sessionbroker

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type operatorAttachmentState struct {
	session *operatormesh.Session
	recipe  OperatorSpec
	result  <-chan error
}

// operatorAttachment retains context ownership, rather than one particular
// network socket. A disconnected generation is closed before the next reserves
// the same endpoint instance and persists its next authorization revision.
// Existing flows end with their carrier; new operations resolve the current
// generation. No transfer is replayed implicitly across a lost connection.
type operatorAttachment struct {
	mu          sync.RWMutex
	current     *operatorAttachmentState
	directory   string
	spec        Spec
	certificate config.CertificateInfo
}

func configuredOperatorCertificate(spec Spec) (config.CertificateInfo, error) {
	cfg, err := loadConfig()
	if err != nil {
		return config.CertificateInfo{}, err
	}
	for _, auth := range cfg.Auth {
		for _, candidate := range auth.Certificates {
			operator, err := operatorPrincipal(candidate)
			if err == nil && operator.Principal() == spec.Operator.Principal && certificateFingerprint(candidate) == spec.CertFingerprint {
				return candidate, nil
			}
		}
	}
	return config.CertificateInfo{}, errOperatorCredentialsChanged
}

func (a *operatorAttachment) open(ctx context.Context, recipe OperatorSpec) (*operatorAttachmentState, error) {
	if err := operatorRecipeCurrent(a.spec); err != nil {
		return nil, err
	}
	certificate, err := configuredOperatorCertificate(a.spec)
	if err != nil {
		return nil, err
	}
	// Open reloads the profile trust/pin state and persists a new revision before
	// dialing. Neither an older key snapshot nor an old proof is reused.
	options := operatormesh.Options{Automatic: recipe.Automatic, StateDirectory: a.directory, Certificate: certificate, RouterAddress: recipe.RouterAddress, RouterUUID: recipe.RouterUUID, Transport: recipe.Transport, BLEPSM: recipe.BLEPSM}
	if a.spec.Operator.Automatic {
		options.Discover = func(ctx context.Context) ([]operatormesh.Options, error) {
			operator, _ := operatorPrincipal(certificate)
			found, err := discoverOperatorLAN(ctx, operator)
			if err != nil {
				return nil, err
			}
			out := make([]operatormesh.Options, 0, len(found))
			for _, candidate := range found {
				next := options
				next.Discover = nil
				next.RouterAddress = candidate.RouterAddress
				next.RouterUUID = candidate.RouterUUID
				next.Transport = candidate.Transport
				next.BLEPSM = candidate.BLEPSM
				out = append(out, next)
			}
			return out, nil
		}
	}
	session, err := openOperatorSession(ctx, options)
	if err != nil {
		return nil, err
	}
	result, err := session.Start(ctx)
	if err != nil {
		session.Close()
		return nil, err
	}
	joining, joined := context.WithTimeout(ctx, operatormesh.JoinSetupTimeout)
	defer joined()
	err = session.WaitForPeer(joining)
	if err != nil {
		session.Close()
		return nil, err
	}
	return &operatorAttachmentState{session: session, recipe: recipe, result: result}, nil
}

func (a *operatorAttachment) snapshot() (*operatorAttachmentState, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.current == nil {
		return nil, status.Error(codes.Unavailable, "operator mesh attachment is reconnecting")
	}
	return a.current, nil
}

func (a *operatorAttachment) replace(next *operatorAttachmentState) *operatorAttachmentState {
	a.mu.Lock()
	previous := a.current
	a.current = next
	a.mu.Unlock()
	return previous
}

func (a *operatorAttachment) adjacent() (*grpc.ClientConn, meshid.Device, error) {
	current, err := a.snapshot()
	if err != nil {
		return nil, meshid.Device{}, err
	}
	peer, live := current.session.AdjacentPeer()
	if !live {
		return nil, meshid.Device{}, status.Error(codes.Unavailable, "adjacent carrier is reconnecting")
	}
	for _, manifest := range current.session.Snapshot().Devices {
		owner, parseErr := manifest.Peer(peer.Tenant())
		if parseErr == nil && owner == peer && manifest.Platform == "Lite" && manifest.Expires > time.Now().UnixMilli() {
			return nil, peer, nil
		}
	}
	client, err := current.session.AgentConnectionForPeer(context.Background(), peer)
	if err != nil {
		return nil, meshid.Device{}, err
	}
	return client.Conn, peer, nil
}

func (a *operatorAttachment) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	current, err := a.snapshot()
	if err != nil {
		return nil, err
	}
	return current.session.DialContext(ctx, network, address)
}

func (a *operatorAttachment) DialAppTLS(ctx context.Context, peer meshid.Device, port uint16) (net.Conn, error) {
	current, err := a.snapshot()
	if err != nil {
		return nil, err
	}
	return current.session.DialAppTLS(ctx, peer, port)
}

func (a *operatorAttachment) run(ctx context.Context) error {
	defer func() {
		if current := a.replace(nil); current != nil {
			current.session.Close()
		}
	}()
	for {
		current, err := a.snapshot()
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-current.result:
		}
		a.replace(nil)
		current.session.Close()
		for attempt := 0; ; attempt++ {
			if err := operatorRecipeCurrent(a.spec); err != nil {
				return err
			}
			if _, err := configuredOperatorCertificate(a.spec); err != nil {
				return err
			}
			timer := time.NewTimer(min(time.Duration(attempt+1)*time.Second, 5*time.Second))
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			// Explicit first-hop intent stays pinned to that recipe. Automatic
			// recipes may rediscover a changed address or a replacement router.
			candidates := []OperatorSpec{current.recipe}
			if a.spec.Operator.Automatic {
				operator, _ := operatorPrincipal(a.certificate)
				found, browseErr := discoverOperatorLAN(ctx, operator)
				if browseErr != nil && ctx.Err() != nil {
					return ctx.Err()
				}
				for _, candidate := range found {
					if candidate.RouterUUID != current.recipe.RouterUUID || candidate.RouterAddress != current.recipe.RouterAddress {
						candidates = append(candidates, candidate)
					}
				}
			}
			for _, candidate := range candidates {
				next, openErr := a.open(ctx, candidate)
				if openErr == nil {
					a.replace(next)
					break
				}
				if ctx.Err() != nil || errors.Is(openErr, errOperatorCredentialsChanged) {
					return openErr
				}
			}
			if _, err := a.snapshot(); err == nil {
				break
			}
		}
	}
}
