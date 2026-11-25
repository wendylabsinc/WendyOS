// Package appsupport holds bounded private configuration readers for local apps.
package appsupport

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"io"
	"os"
)

func ReadJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, authority.MaxEnvelopeBytes+1))
	if err != nil {
		return err
	}
	return authority.DecodeStrict(b, out)
}
func SimulationClient(path string) (client.Config, *client.Client, error) {
	var cfg client.Config
	if err := ReadJSON(path, &cfg); err != nil {
		return cfg, nil, err
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return cfg, nil, errors.New("simulation-only application")
	}
	c, err := client.New(cfg)
	return cfg, c, err
}
