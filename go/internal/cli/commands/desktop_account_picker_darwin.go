//go:build darwin && !ios

package commands

import (
	"fmt"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// DesktopAccountChoice is one saved organization sign-in in the account picker.
type DesktopAccountChoice struct {
	DesktopAccountInfo
}

// DesktopAccountChoices lists the same grouped account rows as DesktopAccounts.
func DesktopAccountChoices() ([]DesktopAccountChoice, error) {
	accounts, err := DesktopAccounts()
	if err != nil {
		return nil, err
	}
	result := make([]DesktopAccountChoice, 0, len(accounts))
	for _, info := range accounts {
		result = append(result, DesktopAccountChoice{DesktopAccountInfo: info})
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
