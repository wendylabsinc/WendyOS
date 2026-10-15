package meshingress

import (
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

func TestRegistryLifecycleAndOwnership(t *testing.T) {
	r := NewRegistry()
	if r.Allowed(8080) || r.Allowed(0) {
		t.Fatal("empty registry authorized a port")
	}
	if err := r.Claim("app_a", 8080); err != nil {
		t.Fatal(err)
	}
	if err := r.Claim("app_a", 8081); err != nil {
		t.Fatal(err)
	}
	if !r.Allowed(8080) || !r.Allowed(8081) || r.Allowed(8082) {
		t.Fatal("wrong authorized port set")
	}
	if err := r.CheckAvailable("app_b", 8080); err == nil {
		t.Fatal("second owner passed availability check")
	}
	if err := r.Claim("app_b", 8080); err == nil {
		t.Fatal("second owner claimed published port")
	}
	r.Release("app_a")
	r.Release("app_a") // stop followed by delete
	if r.Allowed(8080) || r.Allowed(8081) {
		t.Fatal("released owner remained authorized")
	}
	if err := r.Claim("app_b", 8080); err != nil {
		t.Fatalf("new owner could not reuse released port: %v", err)
	}
	if err := r.Claim("", 8083); err == nil {
		t.Fatal("empty owner was accepted")
	}
	if err := r.Claim("app_b", 0); err == nil {
		t.Fatal("port zero was accepted")
	}
}

func TestDialAuthorizedKeepsForwardOwnedUntilDialCompletes(t *testing.T) {
	r := NewRegistry()
	called := false
	if _, err := r.DialAuthorized(8080, func() (net.Conn, error) { called = true; return nil, nil }); !errors.Is(err, ErrPortDenied) || called {
		t.Fatal("unclaimed port reached the dial callback")
	}
	if err := r.Claim("app", 8080); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	unblock := make(chan struct{})
	dialed := make(chan struct{})
	go func() {
		conn, peer := net.Pipe()
		defer peer.Close()
		defer conn.Close()
		_, _ = r.DialAuthorized(8080, func() (net.Conn, error) {
			close(entered)
			<-unblock
			return conn, nil
		})
		close(dialed)
	}()
	<-entered
	released := make(chan struct{})
	go func() { r.Release("app"); close(released) }()
	select {
	case <-released:
		t.Fatal("port was revoked while its authorized dial was in progress")
	case <-time.After(20 * time.Millisecond):
	}
	close(unblock)
	select {
	case <-dialed:
	case <-time.After(time.Second):
		t.Fatal("authorized dial did not finish")
	}
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("release did not finish after dial")
	}
	if r.Allowed(8080) {
		t.Fatal("port remained authorized after release")
	}
}

func TestRegistryConcurrentClaimsHaveOneOwner(t *testing.T) {
	r := NewRegistry()
	var wg sync.WaitGroup
	results := make(chan bool, 64)
	for i := 0; i < cap(results); i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			owner := string(rune('a' + i))
			results <- r.Claim(owner, 9000) == nil
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for ok := range results {
		if ok {
			winners++
		}
	}
	if winners != 1 || !r.Allowed(9000) {
		t.Fatalf("got %d owners for one port", winners)
	}
}
func TestRegistryRecoveryChecksExactContainer(t *testing.T) {
	r := NewRegistry()
	if err := r.Claim("container-a", 18080); err != nil {
		t.Fatal(err)
	}
	if !r.OwnedBy("container-a", 18080) || r.OwnedBy("container-b", 18080) {
		t.Fatal("container ownership did not identify exact running task")
	}
	if err := r.Claim("container-b", 18081); err != nil {
		t.Fatal(err)
	}
	if r.OwnedBy("container-a", 18081) {
		t.Fatal("same-app sibling was mistaken for this container's port")
	}
	r.Release("container-a")
	if r.OwnedBy("container-a", 18080) {
		t.Fatal("stopped app retained container authorization")
	}
}

func TestRegistryCatalogOwnershipFollowsLivePort(t *testing.T) {
	r := NewRegistry()
	if err := r.ClaimForApp("container-a", "app-a", 18080); err != nil {
		t.Fatal(err)
	}
	if !r.AllowedApp("app-a", 18080) || r.AllowedApp("app-b", 18080) {
		t.Fatal("catalog port ownership is not scoped to the running app")
	}
	if err := r.ClaimForApp("container-a", "app-b", 18080); err == nil {
		t.Fatal("live port changed app owner")
	}
	r.Release("container-a")
	if r.AllowedApp("app-a", 18080) {
		t.Fatal("stopped app retained catalog authorization")
	}
}
