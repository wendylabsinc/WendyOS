package commands

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type sdkTestConnector func(context.Context, wendy.DeviceConnectRequest) (wendy.DeviceConnection, error)

func (f sdkTestConnector) ConnectDevice(ctx context.Context, r wendy.DeviceConnectRequest) (wendy.DeviceConnection, error) {
	return f(ctx, r)
}

type sdkTestPeer struct {
	wendy.DeviceConnection
	probe    func(context.Context) (wendy.RPCResponse, error)
	evidence wendy.DeviceConnectionEvidence
	closed   atomic.Int32
}

func (p *sdkTestPeer) Unary(ctx context.Context, _ wendy.RPCRequest) (wendy.RPCResponse, error) {
	return p.probe(ctx)
}
func (p *sdkTestPeer) Evidence() wendy.DeviceConnectionEvidence { return p.evidence }
func (p *sdkTestPeer) Close() error                             { p.closed.Add(1); return nil }
func failedSDKPeer(cause error, org int32) *sdkTestPeer {
	return &sdkTestPeer{probe: func(context.Context) (wendy.RPCResponse, error) {
		return wendy.RPCResponse{}, &deviceRPCError{RPCStatusError: &wendy.RPCStatusError{Code: wendy.RPCUnavailable, Message: cause.Error(), Cause: cause}}
	}, evidence: wendy.DeviceConnectionEvidence{ObservedOrg: org}}
}
func stubSDKConnector(t *testing.T, factory sdkTestConnector) {
	t.Helper()
	old := deviceConnectorFn
	t.Cleanup(func() { deviceConnectorFn = old })
	deviceConnectorFn = func([]config.CertificateInfo, certs.PinChecker) wendy.DeviceConnector { return factory }
}
func awaitSDKSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for candidate")
	}
}

func TestLadderWaitsForRacingCandidatesBeforePlaintext(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	backupStarted, finishBackup := make(chan struct{}), make(chan struct{})
	var plaintextCalled atomic.Bool
	plaintext := grpcclient.NewFromConn(nil)
	stubSDKConnector(t, func(ctx context.Context, r wendy.DeviceConnectRequest) (wendy.DeviceConnection, error) {
		if !r.TLS {
			plaintextCalled.Store(true)
			return &deviceConnection{AgentConnection: plaintext}, nil
		}
		if r.Address == "[fd00::1]:50051" {
			close(backupStarted)
			select {
			case <-finishBackup:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("connection refused")
	})
	result := make(chan error, 1)
	go func() {
		conn, _, err := dialAgentLadderWithCerts(ctx, dialTarget{Addr: "192.0.2.1:50051", Candidates: []string{"192.0.2.1:50051", "[fd00::1]:50051"}}, []config.CertificateInfo{{}})
		if conn != plaintext && err == nil {
			err = errors.New("wrong plaintext connection")
		}
		result <- err
	}()
	awaitSDKSignal(t, backupStarted)
	if plaintextCalled.Load() {
		t.Fatal("plaintext offered before authenticated candidates completed")
	}
	close(finishBackup)
	if err := <-result; err != nil || !plaintextCalled.Load() {
		t.Fatalf("fallback: %v", err)
	}
}

func TestLadderRaceCancellationSkipsPlaintext(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var plaintextCalls atomic.Int32
	peer := failedSDKPeer(context.Canceled, 0)
	peer.probe = func(probeCtx context.Context) (wendy.RPCResponse, error) {
		cancel()
		<-probeCtx.Done()
		return wendy.RPCResponse{}, probeCtx.Err()
	}
	stubSDKConnector(t, func(_ context.Context, r wendy.DeviceConnectRequest) (wendy.DeviceConnection, error) {
		if !r.TLS {
			plaintextCalls.Add(1)
		}
		return peer, nil
	})
	conn, _, err := dialAgentLadderWithCerts(ctx, dialTarget{Addr: "192.0.2.1:50051"}, []config.CertificateInfo{{}})
	if conn != nil || !errors.Is(err, context.Canceled) || plaintextCalls.Load() != 0 || peer.closed.Load() != 1 {
		t.Fatalf("connection=%v error=%v plaintext=%d closed=%d", conn, err, plaintextCalls.Load(), peer.closed.Load())
	}
}
