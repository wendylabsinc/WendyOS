package cbridge_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform/cbridge"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform/cbridge/internal/testhost"
)

func eventually(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("host did not reach expected state")
		}
		time.Sleep(time.Millisecond)
	}
}

func host(t *testing.T, mode int) (*testhost.Host, *cbridge.Host) {
	t.Helper()
	fake := testhost.New(mode)
	h, ok := cbridge.Lookup(fake.ID())
	if !ok {
		t.Fatal("registration missing")
	}
	t.Cleanup(func() {
		fake.Release()
		eventually(t, func() bool { return fake.Stat(4) == 1 })
		if n := fake.Stat(5); n != 0 {
			t.Errorf("C fake rejected %d deliveries", n)
		}
		fake.Free()
	})
	return fake, h
}

func TestSignerAndUnary(t *testing.T) {
	fake, h := host(t, 0)
	if h.Algorithm() != platform.MLDSA65 {
		t.Fatal(h.Algorithm())
	}
	message := []byte{0, 1, 0, 255}
	signature, err := h.Sign(context.Background(), message)
	if err != nil || !bytes.Equal(signature, message) {
		t.Fatalf("signature %v: %v", signature, err)
	}
	if fake.Stat(6) != 1 {
		t.Fatal("wrong signing operation")
	}
	message[1] = 9
	if signature[1] != 1 {
		t.Fatal("signature aliases input")
	}
	key, err := h.PublicKey(context.Background())
	if err != nil || !bytes.Equal(key, []byte{3, 0, 4}) {
		t.Fatalf("key %v: %v", key, err)
	}
	certs, err := h.CertificateChain(context.Background())
	if err != nil || !reflect.DeepEqual(certs, [][]byte{[]byte("leaf"), []byte("issuer")}) {
		t.Fatalf("chain %q: %v", certs, err)
	}
	md := platform.Metadata{{Name: "a-bin", Value: []byte{0, 255}}, {Name: "a-bin", Value: []byte("two")}}
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	r, err := h.Unary(ctx, platform.RPCRequest{Method: "/test.Service/Unary", Message: message, Metadata: md})
	if err != nil || !bytes.Equal(r.Message, message) || !reflect.DeepEqual(r.InitialMetadata, md) {
		t.Fatalf("response %+v: %v", r, err)
	}
	if fake.Stat(7) != uint64(deadline.UnixMilli()) {
		t.Fatal("deadline lost")
	}
	r.InitialMetadata[0].Value[0] = 12
	if md[0].Value[0] != 0 {
		t.Fatal("response metadata aliases input")
	}
	r, err = h.Unary(context.Background(), platform.RPCRequest{})
	if err != nil || len(r.Message) != 0 {
		t.Fatalf("empty protobuf: %+v %v", r, err)
	}
}

func TestStatusMetadataAndCancellationMapping(t *testing.T) {
	_, h := host(t, 1)
	r, err := h.Unary(context.Background(), platform.RPCRequest{})
	var status *platform.RPCStatusError
	if !errors.As(err, &status) || status.Code != platform.RPCPermissionDenied || status.Message != "private remote diagnostic" {
		t.Fatalf("status: %+v", err)
	}
	if bytes.Contains([]byte(err.Error()), []byte("diagnostic")) {
		t.Fatal("error leaked diagnostic")
	}
	if len(r.Message) != 0 || len(r.TrailingMetadata) != 2 || !bytes.Equal(r.TrailingMetadata[0].Value, []byte{0, 255, 0}) {
		t.Fatalf("metadata: %+v", r)
	}
	r.InitialMetadata[0].Value[0] = 9
	r.TrailingMetadata[0].Value[0] = 9
	if status.InitialMetadata[0].Value[0] != 0 || status.TrailingMetadata[0].Value[0] != 0 {
		t.Fatal("error metadata aliases response")
	}
	for _, code := range []uint32{1, 4, 345} {
		fake, h := host(t, 2)
		done := make(chan error, 1)
		go func() { _, err := h.Sign(context.Background(), nil); done <- err }()
		eventually(t, func() bool { return fake.Stat(0) != 0 })
		testhost.Complete(fake.Stat(0), code)
		err := <-done
		if !errors.As(err, &status) || uint32(status.Code) != code {
			t.Fatalf("lost status %d: %v", code, err)
		}
		if code == 1 && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		if code == 4 && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal(err)
		}
	}
}

func TestCancellationAndLateCompletion(t *testing.T) {
	for _, useDeadline := range []bool{false, true} {
		fake, h := host(t, 2)
		ctx, cancel := context.WithCancel(context.Background())
		want := context.Canceled
		if useDeadline {
			cancel()
			ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			want = context.DeadlineExceeded
		}
		done := make(chan error, 1)
		go func() { _, err := h.Unary(ctx, platform.RPCRequest{}); done <- err }()
		eventually(t, func() bool { return fake.Stat(0) != 0 })
		if !useDeadline {
			cancel()
		}
		if err := <-done; !errors.Is(err, want) {
			t.Fatalf("wanted %v, got %v", want, err)
		}
		cancel()
		eventually(t, func() bool { return fake.Stat(3) == 1 })
		testhost.Complete(fake.Stat(0), 7)
		testhost.StaleComplete(fake.Stat(0))
		if testhost.Push(fake.Stat(0)) || testhost.Header(fake.Stat(0)) {
			t.Fatal("late stream event accepted")
		}
	}
	fake, h := host(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.Sign(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if fake.Stat(1) != 0 {
		t.Fatal("started canceled call")
	}
}

func TestStreamDemandOrderingAndOwnership(t *testing.T) {
	fake, h := host(t, 3)
	s, err := h.ServerStream(context.Background(), platform.RPCRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	headers, err := s.Header()
	if err != nil || len(headers) != 2 || headers[0].Name != "trace-bin" {
		t.Fatalf("headers %+v %v", headers, err)
	}
	headers[0].Value[0] = 12
	again, err := s.Header()
	if err != nil || again[0].Value[0] != 0 {
		t.Fatal("headers reused")
	}
	if fake.Stat(2) != 0 {
		t.Fatal("stream started with demand")
	}
	if _, err := s.Recv(); err != nil {
		t.Fatal(err)
	} // valid empty protobuf
	item, err := s.Recv()
	if err != nil || !bytes.Equal(item, []byte{0, 255, 0}) {
		t.Fatalf("item %v %v", item, err)
	}
	if s.Trailer() != nil {
		t.Fatal("trailers visible before terminal Recv")
	}
	for range 2 {
		if _, err := s.Recv(); err != io.EOF {
			t.Fatalf("terminal: %v", err)
		}
	}
	trailers := s.Trailer()
	if len(trailers) != 2 || !bytes.Equal(trailers[0].Value, []byte{0, 255, 0}) {
		t.Fatalf("trailers %+v", trailers)
	}
	trailers[0].Value[0] = 9
	if s.Trailer()[0].Value[0] != 0 {
		t.Fatal("trailers reused")
	}
	if fake.Stat(2) != 2 || fake.Stat(3) != 0 {
		t.Fatal("incorrect demand/cancellation")
	}
}

func TestStreamStatus(t *testing.T) {
	_, h := host(t, 4)
	s, err := h.ServerStream(context.Background(), platform.RPCRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Recv(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, err = s.Recv()
		var status *platform.RPCStatusError
		if !errors.As(err, &status) || status.Code != platform.RPCUnauthenticated || status.InitialMetadata[0].Name != "trace-bin" || len(status.TrailingMetadata) != 2 {
			t.Fatalf("stream status %+v", err)
		}
	}
}

func TestCloseUnblocksStream(t *testing.T) {
	for _, header := range []bool{false, true} {
		fake, h := host(t, 5)
		s, err := h.ServerStream(context.Background(), platform.RPCRequest{})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			if header {
				_, err = s.Header()
			} else {
				_, err = s.Recv()
			}
			done <- err
		}()
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		eventually(t, func() bool { return fake.Stat(3) == 1 })
	}
}

func TestRegistrationRelease(t *testing.T) {
	if testhost.InvalidCreate() != 0 {
		t.Fatal("invalid table accepted")
	}
	for _, mode := range []int{2, 6} {
		fake, h := host(t, mode)
		done := make(chan error, 1)
		go func() { _, err := h.Sign(context.Background(), nil); done <- err }()
		eventually(t, func() bool { return fake.Stat(1) != 0 })
		fake.Release()
		fake.Release()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		eventually(t, func() bool { return fake.Stat(4) == 1 })
		if _, ok := cbridge.Lookup(fake.ID()); ok {
			t.Fatal("released host still registered")
		}
		if _, err := h.Sign(context.Background(), nil); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		testhost.Complete(fake.Stat(0), 0)
	}
}

func TestConcurrentCallsAndCompletionRace(t *testing.T) {
	_, h := host(t, 0)
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			signature, err := h.Sign(context.Background(), []byte{1, 0, 2})
			if err != nil || !bytes.Equal(signature, []byte{1, 0, 2}) {
				t.Errorf("concurrent sign %v %v", signature, err)
			}
		}()
	}
	wg.Wait()
	for range 32 {
		fake, h := host(t, 2)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { _, err := h.Sign(ctx, nil); done <- err }()
		eventually(t, func() bool { return fake.Stat(0) != 0 })
		wg.Add(1)
		go func() { defer wg.Done(); cancel() }()
		wg.Add(1)
		go func() {
			defer wg.Done()
			testhost.Complete(fake.Stat(0), 0)
		}()
		wg.Wait()
		err := <-done
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	}
}

func TestAsyncStreamAndRelease(t *testing.T) {
	fake, h := host(t, 5)
	s, err := h.ServerStream(context.Background(), platform.RPCRequest{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	done := make(chan []byte, 1)
	go func() {
		item, err := s.Recv()
		if err != nil {
			t.Error(err)
		}
		done <- item
	}()
	eventually(t, func() bool { return fake.Stat(2) == 1 })
	if !testhost.Header(fake.Stat(0)) || !testhost.Push(fake.Stat(0)) {
		t.Fatal("async delivery rejected")
	}
	if item := <-done; !bytes.Equal(item, []byte{0, 255, 0}) {
		t.Fatalf("item %v", item)
	}
	fake.Release()
	if _, err := s.Recv(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	eventually(t, func() bool { return fake.Stat(3) == 1 })
}

func TestStreamOpeningError(t *testing.T) {
	_, h := host(t, 1)
	s, err := h.ServerStream(context.Background(), platform.RPCRequest{})
	var status *platform.RPCStatusError
	if s != nil || !errors.As(err, &status) || status.Code != platform.RPCPermissionDenied || len(status.InitialMetadata) != 2 {
		t.Fatalf("opening status: %v %v", s, err)
	}
}
