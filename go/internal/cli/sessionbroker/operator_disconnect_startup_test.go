//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"sync"
	"testing"
	"time"
)

func TestOperatorDisconnectPersistsIntentDuringStartup(t *testing.T) {
	certificate, directory, _, _, _ := operatorIntentFixture(t, false)
	started := make(chan struct{})
	var once sync.Once
	launchOperatorBroker = func(Spec) error { once.Do(func() { close(started) }); return nil }
	joining, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { _, err := EnableOperatorAutomatic(joining, certificate); done <- err }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not enter startup")
	}
	stop, stopCancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	err := DisconnectOperator(stop)
	stopCancel()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("join did not stop")
	}
	operator, _ := operatorPrincipal(certificate)
	recipe, readErr := readOperatorRecipe(operatorRecipePath(directory, operator.Principal()))
	if err != nil || readErr != nil || !recipe.Disconnected {
		t.Fatalf("disconnect while connect holds startup: err=%v readErr=%v persistedDisconnected=%v", err, readErr, recipe.Disconnected)
	}
}

func TestOperatorDisconnectCancelsInitialAttachmentJoin(t *testing.T) {
	certificate, directory, _, _, _ := operatorIntentFixture(t, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered := make(chan struct{})
	ownerDone := make(chan error, 1)
	previousOpen := openOperatorSession
	t.Cleanup(func() { openOperatorSession = previousOpen })
	var workers sync.WaitGroup
	t.Cleanup(func() { cancel(); workers.Wait() })
	openOperatorSession = func(join context.Context, _ operatormesh.Options) (*operatormesh.Session, error) {
		close(entered)
		<-join.Done()
		return nil, join.Err()
	}
	launchOperatorBroker = func(spec Spec) error {
		data, err := json.Marshal(spec)
		if err != nil {
			return err
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			ownerDone <- Run(ctx, base64.RawURLEncoding.EncodeToString(data), time.Second, nil)
		}()
		return nil
	}
	callerDone := make(chan error, 1)
	workers.Add(1)
	go func() { defer workers.Done(); _, err := EnableOperatorAutomatic(ctx, certificate); callerDone <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("initial attachment did not start", ctx.Err())
	}
	stop, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer stopCancel()
	if err := DisconnectOperator(stop); err != nil {
		t.Fatal("disconnect did not join pending owner", err)
	}
	select {
	case err := <-ownerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("pending owner did not observe cancellation", err)
		}
	case <-stop.Done():
		t.Fatal("pending owner outlived disconnect")
	}
	select {
	case err := <-callerDone:
		if !errors.Is(err, errOperatorDisconnected) {
			t.Fatal("joining caller did not observe stopped intent", err)
		}
	case <-stop.Done():
		t.Fatal("joining caller retained startup ownership")
	}
	operator, _ := operatorPrincipal(certificate)
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, operator.Principal()))
	if err != nil || !recipe.Disconnected {
		t.Fatal("pending owner cleared stop intent", err)
	}
}
