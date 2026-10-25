// Package enrolltoken projects unverified token claims through WendyGo.
package enrolltoken

import "github.com/wendylabsinc/wendy-go/enrollment"

// Claims is unverified token data; parsing never grants authority.
type Claims = enrollment.Claims

func Parse(token string) (Claims, error)            { return enrollment.Parse(token) }
func ParseAsset(token string) (int32, int32, error) { return enrollment.ParseAsset(token) }
func TenantSPIFFEURIFromToken(token string) (string, bool) {
	return enrollment.TenantSPIFFEURIFromToken(token)
}
