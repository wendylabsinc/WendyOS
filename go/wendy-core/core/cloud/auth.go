package cloud

import (
	"context"
	"fmt"
	"strings"

	"github.com/wendylabsinc/WendyOS/go/wendy-core/core/identity"
	"github.com/wendylabsinc/WendyOS/go/wendy-core/platform"
)

// AuthenticationMetadata adds a per-RPC DPoP access proof, replacing every
// existing authorization/proof entry without mutating the caller's metadata.
// The host resolves/refreshes the token and bound key before calling.
func AuthenticationMetadata(ctx context.Context, entropy platform.Entropy, authority, method, token string, key platform.Signer, metadata platform.Metadata) (platform.Metadata, error) {
	if token == "" {
		return nil, fmt.Errorf("DPoP: session has no access token")
	}
	if key == nil {
		return nil, fmt.Errorf("DPoP: session has no bound key")
	}
	proof, err := (&identity.Client{Entropy: entropy}).DPoPAccessProof(ctx, key, "POST", "https://"+authority+method, token)
	if err != nil {
		return nil, fmt.Errorf("DPoP: building proof: %w", err)
	}
	result := make(platform.Metadata, 0, len(metadata)+2)
	for _, entry := range metadata {
		if !strings.EqualFold(entry.Name, "authorization") && !strings.EqualFold(entry.Name, "dpop") {
			result = append(result, entry)
		}
	}
	return append(result, platform.MetadataEntry{Name: "authorization", Value: []byte("DPoP " + token)}, platform.MetadataEntry{Name: "dpop", Value: []byte(proof)}), nil
}
