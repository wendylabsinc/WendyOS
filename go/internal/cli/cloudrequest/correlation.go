package cloudrequest

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc/metadata"
)

// CorrelationID adopts the caller's flow ID, or creates one at a new origin.
// Call once per operation and carry it in x-correlation-id on Cloud calls.
// The alphabet and length match Cloud's existing CorrelationContext policy.
func CorrelationID(ctx context.Context) (string, error) {
	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		for _, id := range md.Get("x-correlation-id") {
			if id == "" {
				continue
			}
			if len(id) > 64 || strings.IndexFunc(id, func(r rune) bool {
				return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-')
			}) >= 0 {
				return "", fmt.Errorf("invalid supplied Cloud correlation id")
			}
			return id, nil
		}
	}
	return uuid.NewString(), nil
}
