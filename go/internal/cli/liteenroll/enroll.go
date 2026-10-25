// Package liteenroll adapts CLI sessions, connections and files to WendyGo.
package liteenroll

import (
	"context"
	"errors"
	"net/http"

	wendy "github.com/wendylabsinc/wendy-go"
	sdk "github.com/wendylabsinc/wendy-go/lite/enrollment"
	sdkpb "github.com/wendylabsinc/wendy-go/lite/pb"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudenroll"
	"github.com/wendylabsinc/wendy/go/internal/cli/cloudrequest"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	cloudpb "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func copyMessage(src, dst proto.Message) error {
	wire, err := proto.Marshal(src)
	if err != nil {
		return err
	}
	return proto.Unmarshal(wire, dst)
}
func Config(auth *config.AuthConfig, deviceID, csrURL, timeURL, brokerHost string, brokerPort uint32) (*litepb.WendyConfEnrollment, error) {
	var session *sdk.Session
	if auth != nil {
		session = &sdk.Session{PKIEndpoint: auth.PKIEndpoint, CloudGRPC: auth.CloudGRPC, HasCertificates: len(auth.Certificates) > 0}
		if len(auth.Certificates) > 0 {
			session.PrincipalURI = auth.Certificates[0].PrincipalURI
		}
	}
	cfg, err := sdk.Config(session, deviceID, csrURL, timeURL, brokerHost, brokerPort)
	if err != nil {
		return nil, err
	}
	out := new(litepb.WendyConfEnrollment)
	err = copyMessage(cfg, out)
	return out, err
}
func Mint(ctx context.Context, conn grpc.ClientConnInterface, auth *config.AuthConfig, cfg *litepb.WendyConfEnrollment, name string, hardware *cloudpb.DeviceHardware, profile TrustProfile) (string, error) {
	shared, err := cloudrequest.LiteEnrollmentSigner(ctx, auth)
	if err != nil {
		return "", err
	}
	converted := new(sdkpb.WendyConfEnrollment)
	if err = copyMessage(cfg, converted); err != nil {
		return "", err
	}
	var hw *sdk.DeviceHardware
	if hardware != nil {
		hw = new(sdk.DeviceHardware)
		if err = copyMessage(hardware, hw); err != nil {
			return "", err
		}
	}
	asset, err := sdk.Mint(ctx, cloudrequest.EnrollmentTransport(conn), shared, wendy.CryptoEntropy{}, converted, name, hw, profile.sdk())
	cfg.DeviceId, cfg.Token = converted.DeviceId, converted.Token
	if errors.Is(err, sdk.ErrDeviceUUIDTaken) {
		err = collisionError{err}
	}
	return asset, err
}

type collisionError struct{ error }

func (e collisionError) Is(target error) bool { return target == cloudenroll.ErrDeviceUUIDTaken }
func SignedTime(ctx context.Context, client *http.Client, endpoint, nonce string) ([]byte, error) {
	host, err := wendy.NewLiteHTTPTransport(client)
	if err != nil {
		return nil, err
	}
	return sdk.SignedTime(ctx, host, endpoint, nonce)
}
