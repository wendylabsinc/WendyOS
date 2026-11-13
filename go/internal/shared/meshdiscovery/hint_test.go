package meshdiscovery

import (
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"testing"
)

func TestDefaultHintVectors(t *testing.T) {
	for _, tt := range []struct{ tenant, want string }{
		{"5b8c6c8e-2f9f-4b4a-9a4f-6e2d8c9a0b12", "8e6d83e9d13ffe98"},
		{"00000000-0000-0000-0000-000000000000", "9d688b88d611440b"},
	} {
		tenant, err := meshid.ParseTenant(tt.tenant)
		if err != nil {
			t.Fatal(err)
		}
		if got := DefaultHint(tenant).String(); got != tt.want {
			t.Errorf("tenant %s: %s, want %s", tt.tenant, got, tt.want)
		}
	}
}
