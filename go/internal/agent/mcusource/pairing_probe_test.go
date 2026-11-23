package mcusource

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"go.uber.org/zap"
)

type probeTransport struct {
	manifestOnly
	connect func(context.Context) error
}

func (t probeTransport) Connect(ctx context.Context) error { return t.connect(ctx) }

func TestProbePairingTriesEveryRouteBeforeReturningRefusal(t *testing.T) {
	timeout := errors.New("dial timeout")
	for _, test := range []struct {
		name   string
		routes []error
		want   error
	}{
		{"alternate route connects", []error{ErrSourceRejectedCertificate, nil}, nil},
		{"every route refused", []error{ErrSourceRejectedCertificate, ErrSourceRejectedCertificate}, ErrSourceRejectedCertificate},
		{"alternate route unreachable", []error{ErrSourceRejectedCertificate, timeout}, timeout},
		{"alternate reply is ambiguous", []error{ErrSourceRejectedCertificate, ErrSourceMayHaveRejectedCertificate}, ErrSourceMayHaveRejectedCertificate},
	} {
		t.Run(test.name, func(t *testing.T) {
			addresses := []string{"lan:5054", "mesh:5054"}
			var attempted []string
			s := NewSupervisor(zap.NewNop(), nil, func(_ SensorPairing, addr string) (SensorTransport, error) {
				err := test.routes[len(attempted)]
				attempted = append(attempted, addr)
				return probeTransport{connect: func(context.Context) error { return err }}, nil
			}, nil, nil)
			s.SetSourceResolver(func(context.Context, SensorPairing) ([]string, bool) {
				return addresses, true
			})
			if err := s.ProbePairing(t.Context(), SensorPairing{}, ""); !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
			if !slices.Equal(attempted, addresses) {
				t.Fatalf("tried %v, want %v", attempted, addresses)
			}
		})
	}
}

func TestProbePairingLeavesTimeForAlternateRoutes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
		defer cancel()
		s := NewSupervisor(zap.NewNop(), nil, func(_ SensorPairing, addr string) (SensorTransport, error) {
			return probeTransport{connect: func(ctx context.Context) error {
				if addr == "lan:5054" {
					<-ctx.Done()
				}
				return ctx.Err()
			}}, nil
		}, nil, nil)
		s.SetSourceResolver(func(context.Context, SensorPairing) ([]string, bool) {
			return []string{"lan:5054", "mesh:5054"}, true
		})
		if err := s.ProbePairing(ctx, SensorPairing{}, ""); err != nil {
			t.Fatalf("alternate route could not connect within the probe budget: %v", err)
		}
	})
}
