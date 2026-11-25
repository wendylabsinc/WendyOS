//go:build !linux && !darwin

package authenticatedtime

import "github.com/wendylabsinc/wendy/go/world/production/trustedtime"

func readNative() (trustedtime.Reading, error) {
	return trustedtime.Reading{}, trustedtime.ErrUntrusted
}
