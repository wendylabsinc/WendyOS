package cloudrequest

import (
	"context"
	wendy "github.com/wendylabsinc/wendy-go"
	"github.com/wendylabsinc/wendy-go/cloud"
	"google.golang.org/grpc/metadata"
)

// CorrelationID adopts the caller's flow ID or creates one through WendyGo.
func CorrelationID(ctx context.Context) (string, error) {
	md, _ := metadata.FromOutgoingContext(ctx)
	return cloud.CorrelationID(ctx, coreMetadata(md), wendy.CryptoEntropy{})
}
