//go:build linux

package services

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestWriteJetsonEFIVariableCreatesAndUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test-efivar")
	for _, value := range []uint32{0, 1} {
		payload := jetsonEFIPayload(7, value)
		if err := writeJetsonEFIVariable(path, payload); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, payload) {
			t.Fatalf("variable = % x, want % x", got, payload)
		}
	}
}
