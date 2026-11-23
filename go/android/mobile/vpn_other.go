//go:build !android

package mobile

import "github.com/wendylabsinc/wendy/go/macos/vpn"

func newVPNEngine(config vpn.Config) (*vpn.Engine, error) { return vpn.NewEngine(config) }
