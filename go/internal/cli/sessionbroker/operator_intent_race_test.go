//go:build !windows

package sessionbroker

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

func TestOperatorStalePassiveRecipeCannotClearDisconnectedIntent(t *testing.T) {
	certificate, directory, launches, _, _ := operatorIntentFixture(t, false)
	operator, err := operatorPrincipal(certificate)
	if err != nil {
		t.Fatal(err)
	}
	stale := OperatorSpec{Version: 2, Principal: operator.Principal()}
	spec, err := operatorSpec(stale, certificate)
	if err != nil {
		t.Fatal(err)
	}
	if err = DisconnectOperator(context.Background()); err != nil {
		t.Fatal(err)
	}
	path := operatorRecipePath(directory, operator.Principal())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeOperatorRecipe(context.Background(), spec, path, stale, false); !errors.Is(err, errOperatorDisconnected) {
		t.Fatalf("stale passive writer: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) || *launches != 0 {
		t.Fatalf("passive preparation rewrote stop: launches=%d err=%v", *launches, err)
	}
}

func TestOperatorExplicitResumeWaitsForDisconnectedOwnerDrain(t *testing.T) {
	certificate, directory, launches, _, stopped := operatorIntentFixture(t, true)
	operator, err := operatorPrincipal(certificate)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := brokerDir()
	if err != nil {
		t.Fatal(err)
	}
	if err = ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	socket, _ := paths(dir, operatorKey(operator.Principal()), certs.WendyIdentity{})
	owner, locked, err := acquireIdentityLock(socket)
	if err != nil || !locked {
		t.Fatal(err)
	}
	defer func() {
		if owner != nil {
			releaseLock(owner)
			_ = owner.Close()
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	stopDone := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); stopDone <- DisconnectOperator(ctx) }()
	path := operatorRecipePath(directory, operator.Principal())
	until := time.Now().Add(time.Second)
	for {
		r, e := readOperatorRecipe(path)
		if e == nil && r.Disconnected {
			break
		}
		if time.Now().After(until) {
			t.Fatalf("stop intent was not published: %v", e)
		}
		time.Sleep(time.Millisecond)
	}
	resumeDone := make(chan error, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, e := EnableOperatorAutomatic(ctx, certificate)
		resumeDone <- e
	}()
	select {
	case e := <-resumeDone:
		t.Fatalf("resume passed live old owner: %v", e)
	case <-time.After(30 * time.Millisecond):
	}
	r, e := readOperatorRecipe(path)
	if e != nil || !r.Disconnected {
		t.Fatalf("live old owner stop was cleared: %+v %v", r, e)
	}
	releaseLock(owner)
	_ = owner.Close()
	owner = nil
	select {
	case e := <-stopDone:
		if e != nil {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("stop/recipe lock deadlock")
	}
	select {
	case e := <-resumeDone:
		if !errors.Is(e, stopped) {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("resume/recipe lock deadlock")
	}
	if *launches != 1 {
		t.Fatalf("resume launches=%d", *launches)
	}
}
