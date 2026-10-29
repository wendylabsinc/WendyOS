//go:build !linux

package nancontrol

import "errors"

func GrantSocketAccess(string, int) error { return errors.New("NAN control requires Linux") }
func RestoreSocketAccess(string) error    { return nil }
