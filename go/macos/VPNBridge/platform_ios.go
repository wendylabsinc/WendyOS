//go:build ios

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/macos/backend"
	"github.com/wendylabsinc/wendy/go/macos/vpn"
)

var mobileBackend = backend.New()

func platformConfig(cfg vpn.Config) (vpn.Config, error) {
	if !filepath.IsAbs(cfg.BootstrapDirectory) {
		return vpn.Config{}, errors.New("missing shared mobile profile")
	}
	if err := os.Setenv("WENDY_CONFIG_DIR", cfg.BootstrapDirectory); err != nil {
		return vpn.Config{}, err
	}
	if err := mobileBackend.Initialize(""); err != nil {
		return vpn.Config{}, err
	}
	if err := sessionbroker.SetEmbeddedActive(context.Background(), true); err != nil {
		return vpn.Config{}, err
	}
	return mobileBackend.StartVPN(cfg.Routes, cfg.Gateway)
}
func stopPlatform() {
	_ = mobileBackend.StopVPN()
	_ = sessionbroker.SetEmbeddedActive(context.Background(), false)
}
