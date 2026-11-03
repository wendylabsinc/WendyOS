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
	GetLegacyOrganizationId() int32
}

func Key(t Identity) string {
	if t.GetAssetId() != 0 || t.GetLegacyOrganizationId() != 0 {
		return fmt.Sprintf("legacy-%d-%d", t.GetLegacyOrganizationId(), t.GetAssetId())
	}
	return t.GetAssetUuid()
}
func Result(key string, delivered bool, message string) *pb.DeliveryResult {
	r := &pb.DeliveryResult{AssetUuid: key, Delivered: delivered, Error: message}
	if rest, ok := strings.CutPrefix(key, "legacy-"); ok {
		org, asset, ok := strings.Cut(rest, "-")
		if !ok {
			return r
		}
		o, e1 := strconv.ParseInt(org, 10, 32)
		a, e2 := strconv.ParseInt(asset, 10, 32)
		if e1 == nil && e2 == nil && o >= 0 && a > 0 {
			r.AssetUuid = ""
			r.AssetId = int32(a)
			r.LegacyOrganizationId = int32(o)
		}
	}
	return r
}
