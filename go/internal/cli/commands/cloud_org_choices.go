package commands

import (
	"context"
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

type cloudOrgChoice struct {
	key, id, name string
	source        *config.AuthConfig
}

// Keep stored identities visible even if a membership query only returns the
// requesting certificate's tenant, or an environment is temporarily offline.
// IDs are scoped by endpoint so selection cannot cross cloud environments.
func cloudOrgChoices(ctx context.Context, cfg *config.Config) ([]cloudOrgChoice, []error) {
	var choices []cloudOrgChoice
	var failures []error
	index := map[string]int{}
	add := func(a *config.AuthConfig, id, name string) {
		key := a.CloudGRPC + "::" + id
		if i, ok := index[key]; ok {
			if name != "" {
				choices[i].name = name
			}
			return
		}
		if name == "" {
			name = "org " + id
		}
		index[key] = len(choices)
		choices = append(choices, cloudOrgChoice{key: key, id: id, name: name, source: a})
	}
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		if len(a.Certificates) > 0 {
			add(a, a.OrganizationKey(), cachedCloudOrganizationName(a))
		}
	}
	seen := map[string]bool{}
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		if len(a.Certificates) == 0 || seen[authSessionKey(a)] {
			continue
		}
		seen[authSessionKey(a)] = true
		a = cloudAuthForSessionKey(cfg, authSessionKey(a))
		queryCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var err error
		if a.Certificates[0].TenantUUID() != "" {
			orgs, e := listCloudOrganizationsV2(queryCtx, a)
			err = e
			for _, org := range orgs {
				if org != nil {
					add(a, org.GetId(), org.GetName())
				}
			}
		} else {
			orgs, e := listOrgsFromCloud(queryCtx, a)
			err = e
			for _, org := range orgs {
				if org != nil {
					add(a, fmt.Sprint(org.GetId()), org.GetName())
				}
			}
		}
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf("listing organizations for %s: %w", authSessionLabel(a), err))
		}
	}
	return choices, failures
}

func cloudOrgChoiceItems(choices []cloudOrgChoice) []tui.PickerItem {
	items := make([]tui.PickerItem, 0, len(choices))
	for _, choice := range choices {
		items = append(items, tui.PickerItem{Name: choice.name, Description: choice.id, Type: choice.source.CloudGRPC, DedupKey: choice.key, Value: choice.key})
	}
	return items
}

var pickCloudOrgChoice = func(cfg *config.Config, choices []cloudOrgChoice) (string, error) {
	// Reuse saved-context defaults and their persistence behavior.
	picker := newAuthSessionPicker(cfg)
	model, _ := picker.Update(tui.PickerSetMsg{Items: cloudOrgChoiceItems(choices)})
	picker = model.(tui.PickerModel)
	model, _ = picker.Update(tui.PickerDoneMsg{})
	picker = model.(tui.PickerModel)
	picker.FocusItem(picker.DefaultKey)
	final, err := tea.NewProgram(picker).Run()
	if err != nil {
		return "", err
	}
	result := final.(tui.PickerModel)
	if result.Cancelled() {
		return "", ErrUserCancelled
	}
	if result.Selected() == nil {
		return "", fmt.Errorf("no organisation selected")
	}
	return result.Selected().Value.(string), nil
}

func cloudAuthForSessionKey(cfg *config.Config, key string) *config.AuthConfig {
	var selected *config.AuthConfig
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		if authSessionKey(a) == key && (selected == nil || (selected.OAuthIssuer == "" && a.OAuthIssuer != "")) {
			selected = a
		}
	}
	return selected
}

func switchCloudOrganizationAcrossContexts(ctx context.Context, cfg *config.Config) (*config.AuthConfig, *config.Config, error) {
	choices, failures := cloudOrgChoices(ctx, cfg)
	if err := ctx.Err(); err != nil {
		return nil, cfg, err
	}
	for _, failure := range failures {
		cliLogln("Warning: %v; keeping stored organisation contexts available.", failure)
	}
	key, err := pickCloudOrgChoice(cfg, choices)
	if err != nil {
		return nil, cfg, err
	}
	var selected *cloudOrgChoice
	for i := range choices {
		if choices[i].key == key {
			selected = &choices[i]
			break
		}
	}
	if selected == nil {
		return nil, cfg, fmt.Errorf("selected organisation is no longer available")
	}
	if auth := cloudAuthForSessionKey(cfg, key); auth != nil {
		if fresh, e := loadCloudOrgConfig(); e == nil {
			if a := cloudAuthForSessionKey(fresh, key); a != nil {
				return a, fresh, nil
			}
		}
		return auth, cfg, nil
	}
	fmt.Println(tui.InfoMessage(fmt.Sprintf("No credentials are stored for %s (%s). Complete login and select that organization in the browser.", selected.name, selected.id)))
	if err := relogin(ctx, selected.source); err != nil {
		return nil, cfg, err
	}
	fresh, err := loadCloudOrgConfig()
	if err != nil {
		return nil, cfg, err
	}
	if auth := cloudAuthForSessionKey(fresh, key); auth != nil {
		return auth, fresh, nil
	}
	return nil, fresh, fmt.Errorf("login completed without credentials for selected organization %s", selected.id)
}
