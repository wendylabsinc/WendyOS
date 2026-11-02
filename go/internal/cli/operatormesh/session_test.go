package operatormesh

import (
	"sync"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshoperator"
)

func TestOperatorEndpointStateNormalRestartAndContexts(t *testing.T) {
	operator, err := meshoperator.ParsePrincipal("spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/auth0%7Calice")
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	first, release, err := reserveEndpoint(directory, operator)
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	next, release, err := reserveEndpoint(directory, operator)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if first.Revision != 1 || next.Revision != 2 || first.Instance != next.Instance {
		t.Fatal("restart changed instance or failed to advance durable revision")
	}
	otherDirectory := t.TempDir()
	other, release, err := reserveEndpoint(otherDirectory, operator)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if other.Instance == first.Instance {
		t.Fatal("separate CLI installation reused an endpoint instance")
	}
	// Ordinary concurrent invocations serialize allocation through the portable
	// file lock; the future broker can retain one of these owners for all clients.
	const callers = 4
	outcomes := make(chan endpointState, callers)
	failures := make(chan error, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			state, release, err := reserveEndpoint(directory, operator)
			if err != nil {
				failures <- err
				return
			}
			outcomes <- state
			release()
		}()
	}
	wg.Wait()
	close(outcomes)
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	revisions := map[uint64]bool{}
	for state := range outcomes {
		if state.Instance != first.Instance {
			t.Fatal("concurrent allocation changed persistent instance")
		}
		revisions[state.Revision] = true
	}
	if len(revisions) != callers {
		t.Fatal("concurrent invocations repeated a revision")
	}
}
