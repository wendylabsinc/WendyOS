package liteenroll

import (
	"context"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/wendylabsinc/wendy-go/lite/enrollment"
	sdkpb "github.com/wendylabsinc/wendy-go/lite/pb"
	litepb "github.com/wendylabsinc/wendy/go/proto/gen/litepb"
)

func DiscoverTrust(ctx context.Context, cfg *litepb.WendyConfEnrollment, endpoint string) (*http.Client, error) {
	return DiscoverTrustForProfile(ctx, cfg, endpoint, TrustProfile{})
}
func DiscoverTrustForProfile(ctx context.Context, cfg *litepb.WendyConfEnrollment, endpoint string, p TrustProfile) (*http.Client, error) {
	c := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return fmt.Errorf("CA discovery redirects are not allowed")
	}}
	if err := discoverTrustForProfile(ctx, c, cfg, endpoint, p); err != nil {
		return nil, err
	}
	return trustHTTPClient(cfg.HttpsRoots)
}
func discoverTrust(ctx context.Context, c *http.Client, cfg *litepb.WendyConfEnrollment, endpoint string) error {
	return discoverTrustForProfile(ctx, c, cfg, endpoint, TrustProfile{})
}
func discoverTrustForProfile(ctx context.Context, c *http.Client, cfg *litepb.WendyConfEnrollment, endpoint string, p TrustProfile) error {
	host, err := newDiscoveryHTTP(c, "CA discovery redirects are not allowed")
	if err != nil {
		return err
	}
	converted := new(sdkpb.WendyConfEnrollment)
	if err = copyMessage(cfg, converted); err != nil {
		return err
	}
	if err = sdk.DiscoverTrustForProfile(ctx, host, converted, endpoint, p.sdk()); err != nil {
		return err
	}
	return copyMessage(converted, cfg)
}
