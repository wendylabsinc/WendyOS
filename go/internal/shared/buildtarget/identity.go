// Package buildtarget keeps legacy numeric build destinations separate from
// PKI UUID destinations throughout delivery and per-device result accounting.
package buildtarget

import (
	"fmt"
	"strconv"
	"strings"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

type Identity interface {
	GetAssetUuid() string
	GetAssetId() int32
}

func Key(t Identity) string {
	if t.GetAssetId() != 0 {
		return fmt.Sprintf("legacy-%d", t.GetAssetId())
	}
	return t.GetAssetUuid()
}
func Result(key string, delivered bool, message string) *pb.DeliveryResult {
	r := &pb.DeliveryResult{AssetUuid: key, Delivered: delivered, Error: message}
	if rest, ok := strings.CutPrefix(key, "legacy-"); ok {
		a, err := strconv.ParseInt(rest, 10, 32)
		if err == nil && a > 0 {
			r.AssetUuid = ""
			r.AssetId = int32(a)
		}
	}
	return r
}
