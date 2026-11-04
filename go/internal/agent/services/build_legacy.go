package services

import (
	"crypto/tls"
	"fmt"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func (s *BuildService) resolvePushTarget(target *pb.PushTarget) (DeliveryTarget, error) {
	if target.GetAssetId() != 0 {
		if target.GetAssetUuid() != "" || target.GetAssetId() <= 0 {
			return DeliveryTarget{}, fmt.Errorf("invalid or mixed legacy build target")
		}
		if s.resolveLegacyTarget == nil {
			return DeliveryTarget{}, fmt.Errorf("build host does not support legacy delivery")
		}
		return s.resolveLegacyTarget(0, target.GetAssetId())
	}
	return s.resolveTarget(target.GetAssetUuid())
}
func (s *BuildService) deliveryTLS(target DeliveryTarget) (*tls.Config, error) {
	if target.LegacyOrgID != 0 || target.LegacyAssetID != 0 {
		if target.Device.IsValid() || target.AssetUUID != "" || target.LegacyOrgID <= 0 || target.LegacyAssetID <= 0 || s.legacyPushTLS == nil {
			return nil, fmt.Errorf("invalid legacy delivery credentials")
		}
		return s.legacyPushTLS(target.LegacyOrgID, target.LegacyAssetID)
	}
	if s.pushTLS == nil {
		return nil, fmt.Errorf("missing delivery credentials")
	}
	return s.pushTLS(target.Device)
}
