//go:build darwin && !ios

package commands

import (
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type DesktopAccountChoice struct {
	DesktopAccountInfo
	OrganizationID, OrganizationName string
}

func DesktopAccountChoices() ([]DesktopAccountChoice, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	current, _ := config.ResolveAuth(cfg, "", nil)
	result := make([]DesktopAccountChoice, 0, len(cfg.Auth))
	for _, auth := range cfg.Auth {
		info := desktopAccountInfo(auth)
		info.Active = current != nil && current.Name == auth.Name
		organization := ""
		if len(auth.Certificates) > 0 {
			organization = auth.OrganizationKey()
		}
		result = append(result, DesktopAccountChoice{DesktopAccountInfo: info,
			OrganizationID: organization, OrganizationName: config.OrganizationName(info.Cloud, organization)})
	}
	return result, nil
}

// Select an exact saved context, with the same certificate requirement as auth
// use. Updating under the configuration lock preserves concurrent token refreshes.
func DesktopUseAccount(name string) error {
	return config.Update(func(cfg *config.Config) (bool, error) {
		selected, ok := cfg.ContextByName(name)
		if !ok {
			return false, fmt.Errorf("this saved account is no longer available; sign in again")
		}
		if len(selected.Certificates) == 0 {
			return false, fmt.Errorf("this account needs to sign in again before it can be selected")
		}
		changed := cfg.CurrentContext != selected.Name
		cfg.CurrentContext = selected.Name
		return changed, nil
	})
}
