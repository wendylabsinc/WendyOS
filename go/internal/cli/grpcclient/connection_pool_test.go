package grpcclient

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
)

type poolAgent struct {
	pb.UnimplementedWendyAgentServiceServer
	entered chan struct{}
	finish  chan struct{}
}

func (s *poolAgent) GetAgentVersion(ctx context.Context, _ *pb.GetAgentVersionRequest) (*pb.GetAgentVersionResponse, error) {
	s.entered <- struct{}{}
	select {
	case <-s.finish:
		return &pb.GetAgentVersionResponse{}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestConnectionPoolMultiplexesRPCsAndRetainsOtherUsers(t *testing.T) {
	var pool ConnectionPool
	t.Cleanup(pool.Close)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	agent := &poolAgent{entered: make(chan struct{}, 2), finish: make(chan struct{})}
	pb.RegisterWendyAgentServiceServer(server, agent)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	var dials atomic.Int32
	dial := func(ctx context.Context) (*AgentConnection, error) {
		dials.Add(1)
		conn, err := grpc.NewClient("passthrough:///pooled", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
		if err != nil {
			return nil, err
		}
		return NewFromConn(conn), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	one, err := pool.Acquire(ctx, "device", dial)
	if err != nil {
		t.Fatal(err)
	}
	two, err := pool.Lookup(ctx, "device")
	if err != nil {
		t.Fatal(err)
	}
	if one.Conn != two.Conn || dials.Load() != 1 {
		t.Fatal("did not share gRPC channel")
	}
	results := make(chan error, 2)
	for _, conn := range []*AgentConnection{one, two} {
		go func() { _, err := conn.AgentService.GetAgentVersion(ctx, &pb.GetAgentVersionRequest{}); results <- err }()
	}
	for range 2 {
		select {
		case <-agent.entered:
		case <-ctx.Done():
			t.Fatal("RPCs were serialized")
		}
	}
	one.Close()
	one.Close() // A view can run cleanup more than once.
	if two.Conn.GetState() == connectivity.Shutdown {
		t.Fatal("closing one user closed another's channel")
	}
	close(agent.finish)
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	two.Close()
	three, err := pool.Acquire(ctx, "device", dial)
	if err != nil {
		t.Fatal(err)
	}
	if three.Conn != two.Conn || dials.Load() != 1 {
		t.Fatal("reopening did not reuse idle channel")
	}
	pool.Close()
	if three.Conn.GetState() != connectivity.Shutdown {
		t.Fatal("scope teardown retained active channel")
	}
	three.Close()
	if _, err := pool.Acquire(ctx, "device", dial); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestConnectionPoolSharesSetupWithoutFirstCallerCancellation(t *testing.T) {
	var pool ConnectionPool
	t.Cleanup(pool.Close)
	started, finish := make(chan struct{}), make(chan struct{})
	var dials atomic.Int32
	dial := func(ctx context.Context) (*AgentConnection, error) {
		if dials.Add(1) == 1 {
			close(started)
		}
		select {
		case <-finish:
			return &AgentConnection{}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, stop := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() { _, err := pool.Acquire(first, "device", dial); result <- err }()
	<-started
	// Reserve the second user before cancelling the first.
	second := make(chan *AgentConnection, 1)
	go func() { conn, _ := pool.Lookup(ctx, "device"); second <- conn }()
	deadline := time.Now().Add(time.Second)
	for {
		pool.mu.Lock()
		users := pool.entries["device"].users
		pool.mu.Unlock()
		if users == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("second waiter did not arrive")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	close(finish)
	conn := <-second
	if conn == nil || dials.Load() != 1 {
		t.Fatal("first caller cancelled shared setup")
	}
	conn.Close()
}

type poolCloser struct{ closed *atomic.Int32 }

func (c poolCloser) Close() error { c.closed.Add(1); return nil }

func TestConnectionPoolScopeAndConsumerHelpers(t *testing.T) {
	var pool ConnectionPool
	var transports, helpers atomic.Int32
	dial := func(context.Context) (*AgentConnection, error) {
		return &AgentConnection{ExtraClosers: []io.Closer{poolCloser{&transports}}}, nil
	}
	one, _ := pool.Acquire(context.Background(), "account-a/device", dial)
	two, _ := pool.Acquire(context.Background(), "account-b/device", dial)
	one.AddCloser(poolCloser{&helpers})
	one.Close()
	if helpers.Load() != 1 || transports.Load() != 0 {
		t.Fatal("consumer helper or transport lifetime incorrect")
	}
	pool.Close()
	two.Close()
	if transports.Load() != 2 {
		t.Fatal("account channels were shared or left open")
	}
}

func TestConnectionPoolCancelsAbandonedSetupAndAllowsRetry(t *testing.T) {
	var pool ConnectionPool
	t.Cleanup(pool.Close)
	started, stopped := make(chan struct{}), make(chan struct{})
	dial := func(ctx context.Context) (*AgentConnection, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := pool.Acquire(ctx, "device", dial); done <- err }()
	<-started
	cancel()
	<-done
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("abandoned setup kept dialing")
	}
	conn, err := pool.Acquire(context.Background(), "device", func(context.Context) (*AgentConnection, error) { return &AgentConnection{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestConnectionPoolIdleExpiryIgnoresPreviousIdlePeriod(t *testing.T) {
	var pool ConnectionPool
	t.Cleanup(pool.Close)
	var closed atomic.Int32
	dial := func(context.Context) (*AgentConnection, error) {
		return &AgentConnection{ExtraClosers: []io.Closer{poolCloser{&closed}}}, nil
	}
	one, err := pool.Acquire(context.Background(), "device", dial)
	if err != nil {
		t.Fatal(err)
	}
	one.Close()
	pool.mu.Lock()
	entry, oldGeneration := pool.entries["device"], pool.entries["device"].idleGeneration
	pool.mu.Unlock()
	two, err := pool.Acquire(context.Background(), "device", dial)
	if err != nil {
		t.Fatal(err)
	}
	// Model an expired timer whose callback was waiting for the pool mutex.
	pool.expire("device", entry, oldGeneration)
	if closed.Load() != 0 {
		t.Fatal("idle expiry closed an active channel")
	}
	two.Close()
	pool.expire("device", entry, oldGeneration)
	pool.mu.Lock()
	current := pool.entries["device"]
	newGeneration := entry.idleGeneration
	pool.mu.Unlock()
	if current != entry {
		t.Fatal("previous idle period evicted the reopened channel")
	}
	pool.expire("device", entry, newGeneration)
	deadline := time.Now().Add(time.Second)
	for closed.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("idle transport was not closed")
		}
		time.Sleep(time.Millisecond)
	}
	pool.mu.Lock()
	remaining := len(pool.entries)
	pool.mu.Unlock()
	if remaining != 0 {
		t.Fatal("expired channel remained reusable")
	}
}

func TestConnectionPoolScopeCloseRetiresReplacedActiveChannels(t *testing.T) {
	var pool ConnectionPool
	t.Cleanup(pool.Close)
	var closed atomic.Int32
	dial := func(context.Context) (*AgentConnection, error) {
		channel, err := grpc.NewClient("passthrough:///unused", grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			return nil, err
		}
		conn := NewFromConn(channel)
		conn.AddCloser(poolCloser{&closed})
		return conn, nil
	}
	one, err := pool.Acquire(context.Background(), "device", dial)
	if err != nil {
		t.Fatal(err)
	}
	// A dead channel can be replaced while another view still owns its handle.
	one.Conn.Close()
	two, err := pool.Acquire(context.Background(), "device", dial)
	if err != nil {
		t.Fatal(err)
	}
	if one.Conn == two.Conn {
		t.Fatal("shutdown channel was reused")
	}
	pool.Close()
	if closed.Load() != 2 || two.Conn.GetState() != connectivity.Shutdown {
		t.Fatal("scope teardown did not close both current and retired channels")
	}
	one.Close()
	two.Close()
}

func TestConnectionPoolScopeCloseWakesPendingCallerAndClosesLateTransport(t *testing.T) {
	var pool ConnectionPool
	t.Cleanup(pool.Close)
	started, finish, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	dial := func(context.Context) (*AgentConnection, error) {
		close(started)
		// Some connectors finish an in-flight handshake after cancellation.
		<-finish
		return &AgentConnection{ExtraClosers: []io.Closer{poolCloseFunc(func() error { close(closed); return nil })}}, nil
	}
	result := make(chan error, 1)
	go func() { _, err := pool.Acquire(context.Background(), "device", dial); result <- err }()
	<-started
	pool.Close()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("closed scope did not wake pending caller")
	}
	close(finish)
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("late transport leaked after scope teardown")
	}
}

type poolCloseFunc func() error

func (f poolCloseFunc) Close() error { return f() }

func TestConnectionPoolLookupMissDoesNotCreateEntry(t *testing.T) {
	var pool ConnectionPool
	conn, err := pool.Lookup(context.Background(), "missing")
	if conn != nil || err != nil {
		t.Fatalf("miss = %v, %v", conn, err)
	}
	if len(pool.entries) != 0 || len(pool.all) != 0 {
		t.Fatal("lookup miss created an entry")
	}
	pool.Close()
	if _, err := pool.Lookup(context.Background(), "missing"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestConnectionPoolRefreshKeepsUsersAndSharesReplacement(t *testing.T) {
	var pool ConnectionPool
	t.Cleanup(pool.Close)
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	finish := make(chan struct{})
	close(finish)
	agent := &poolAgent{entered: make(chan struct{}, 2), finish: finish}
	pb.RegisterWendyAgentServiceServer(server, agent)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	var dials atomic.Int32
	dial := func(ctx context.Context) (*AgentConnection, error) {
		dials.Add(1)
		channel, err := grpc.NewClient("passthrough:///refresh", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }))
		if err != nil {
			return nil, err
		}
		conn := NewFromConn(channel)
		if _, err := conn.AgentService.GetAgentVersion(ctx, &pb.GetAgentVersionRequest{}); err != nil {
			conn.Close()
			return nil, err
		}
		return conn, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	one, err := pool.Acquire(ctx, "device", dial)
	if err != nil {
		t.Fatal(err)
	}
	if one.Conn.GetState() != connectivity.Ready {
		t.Fatal("initial connection is not ready")
	}
	two, err := pool.Refresh(ctx, "device", one, dial)
	if err != nil {
		t.Fatal(err)
	}
	if one.Conn == two.Conn || dials.Load() != 2 {
		t.Fatal("refresh reused ready channel")
	}
	// A second recovery of the same stale channel, after the first finished,
	// joins its replacement instead of evicting it.
	again, err := pool.Refresh(ctx, "device", one, dial)
	if err != nil {
		t.Fatal(err)
	}
	if again.Conn != two.Conn || dials.Load() != 2 {
		t.Fatal("repeated refresh of a stale channel replaced its replacement")
	}
	again.Close()
	if one.Conn.GetState() != connectivity.Ready {
		t.Fatal("refresh interrupted existing user")
	}
	three, err := pool.Lookup(ctx, "device")
	if err != nil || three == nil {
		t.Fatalf("lookup = %v, %v", three, err)
	}
	if three.Conn != two.Conn {
		t.Fatal("new opens did not reuse replacement")
	}
	pool.Close()
	if one.Conn.GetState() != connectivity.Shutdown || two.Conn.GetState() != connectivity.Shutdown {
		t.Fatal("scope close retained current or replaced channel")
	}
	one.Close()
	two.Close()
	three.Close()
}
