package mcusource

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// ProbePairing checks whether the saved source can authenticate this agent.
// Trying the same routes as streaming prevents a stale first endpoint from
// making the preflight reject a source that is reachable another way.
func (s *Supervisor) ProbePairing(ctx context.Context, pairing SensorPairing, addr string) error {
	var addresses []string
	if addr != "" {
		addresses = append(addresses, addr)
	}
	var discoveryErr error
	if addr == "" || pairing.SourcePrincipalURI != "" {
		resolve := s.resolveSource
		if resolve == nil {
			resolve = resolvePairingLANAddrs
		}
		resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resolved, ok := resolve(resolveCtx, pairing)
		cancel()
		if !ok || len(resolved) == 0 {
			discoveryErr = errors.New("sensor source discovery did not confirm current routes")
			if len(addresses) == 0 {
				return discoveryErr
			}
		}
		// Principal pairings use the supplied address as a first-attempt
		// hint. Their supervisor may rediscover a different working route.
		for _, candidate := range resolved {
			if !slices.Contains(addresses, candidate) {
				addresses = append(addresses, candidate)
			}
		}
	}

	var refusal, inconclusive error
	for i, address := range addresses {
		if err := ctx.Err(); err != nil {
			return err
		}
		attempt, cancel := ctx, func() {}
		if deadline, ok := ctx.Deadline(); ok && i+1 < len(addresses) {
			// Leave time for alternate routes within the bounded save-time probe.
			attempt, cancel = context.WithTimeout(ctx, time.Until(deadline)/time.Duration(len(addresses)-i))
		}
		err := s.probeAddress(attempt, pairing, address)
		cancel()
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrSourceRejectedCertificate):
			refusal = err
		case inconclusive == nil || errors.Is(err, ErrSourceMayHaveRejectedCertificate):
			inconclusive = err
		}
	}
	// An explicit refusal only settles the probe if every route refused.
	if inconclusive != nil {
		return inconclusive
	}
	if discoveryErr != nil {
		return fmt.Errorf("%w: %v", discoveryErr, refusal)
	}
	return refusal
}

func (s *Supervisor) probeAddress(ctx context.Context, pairing SensorPairing, addr string) error {
	transport, err := s.transportFor(pairing, addr)
	if err != nil {
		return err
	}
	defer transport.Close()
	if connector, ok := transport.(interface{ Connect(context.Context) error }); ok {
		return connector.Connect(ctx)
	}
	return nil
}
