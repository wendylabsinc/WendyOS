package localmesh

import (
	"path/filepath"
	"testing"

	"github.com/wendylabsinc/WendyOS/babel"
)

func TestStateStoreRestartAndIdentityBinding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, state, err := OpenStateStore(path, 64, 445)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := RouterID(64, 445)
	engine, _ := babel.New(babel.Config{RouterID: id})
	cp := engine.Checkpoint()
	state.Revision = 123
	state.Babel = &cp
	if err = s.Save(state); err != nil {
		t.Fatal(err)
	}
	_, restored, err := OpenStateStore(path, 64, 445)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Revision != 123 || restored.Babel.RouterID != id {
		t.Fatal(restored)
	}
	if _, _, err = OpenStateStore(path, 65, 445); err == nil {
		t.Fatal("cross-org safety state reused")
	}
}
