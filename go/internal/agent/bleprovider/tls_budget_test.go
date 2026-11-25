package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"
)

func TestMeshTLSWaitsForBusyRemoteAdapter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		serverConfig, clientConfig := metricsTLSConfigs(t)
		serverConfig.SessionTicketsDisabled = true
		clientConfig.SessionTicketsDisabled = true
		a, b := net.Pipe()
		defer a.Close()
		defer b.Close()
		done := make(chan error, 1)
		go func() {
			time.Sleep(20 * time.Second)
			done <- tls.Server(b, serverConfig).HandshakeContext(context.Background())
		}()
		elapsed, err := handshakeMeshOutbound(context.Background(), tls.Client(a, clientConfig))
		if err != nil {
			a.Close()
			b.Close()
			<-done
			t.Fatal(err)
		}
		if elapsed < 20*time.Second {
			t.Fatalf("elapsed %s", elapsed)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestMeshTLSHonorsCancellationAndBudget(t *testing.T) {
	for _, budget := range []time.Duration{2 * time.Second, meshTLSOutboundTimeout} {
		synctest.Test(t, func(t *testing.T) {
			_, clientConfig := metricsTLSConfigs(t)
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			elapsed, err := handshakeMeshOutbound(ctx, tls.Client(a, clientConfig))
			if !errors.Is(err, context.DeadlineExceeded) || elapsed != budget {
				t.Fatalf("elapsed=%s error=%v", elapsed, err)
			}
		})
	}
}
