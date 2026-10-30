package main

import (
	"errors"
	"runtime"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

func startPlatformVPN(routes []vpn.Route, gateway string) (vpn.Config, error) {
	if runtime.GOOS != "ios" {
		return desktop.StartVPN(routes, gateway)
	}
	if len(routes) == 0 && gateway == "" {
		return vpn.Config{}, errors.New("select devices or an internet gateway")
	}
	var err error
	if len(routes) > 0 {
		routes, err = vpn.BuildRoutes(routes)
		if err != nil {
			return vpn.Config{}, err
		}
	}
	directory, err := config.ConfigDir()
	if err != nil {
		return vpn.Config{}, err
	}
	if err = desktop.StopVPN(); err != nil {
		return vpn.Config{}, err
	}
	sessionbroker.SuspendEmbedded()
	return vpn.Config{Routes: routes, Gateway: gateway, BootstrapDirectory: directory}, nil
}
func stopPlatformVPN() error {
	err := desktop.StopVPN()
	if runtime.GOOS == "ios" {
		sessionbroker.ResumeEmbedded()
	}
	return err
}
