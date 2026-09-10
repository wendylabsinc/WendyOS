//go:build darwin || linux || windows

package commands

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/cli/tui"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// authSessionLabel renders a session for humans: "org <N> — <endpoint>", or
// just the endpoint when the session holds no certificate (and thus no org).
func authSessionLabel(a *config.AuthConfig) string {
	if len(a.Certificates) > 0 {
		if name := cachedCloudOrganizationName(a); name != "" {
			return fmt.Sprintf("%s — %s", name, a.CloudGRPC)
		}
		return fmt.Sprintf("org %s — %s", a.OrganizationKey(), a.CloudGRPC)
	}
	if a.OAuthIssuer != "" {
		return fmt.Sprintf("%s — %s", issuerRealm(a.OAuthIssuer), a.CloudGRPC)
	}
	return a.CloudGRPC
}

// authSessionKey returns a unique string identifying an auth session: the gRPC
// endpoint combined with the cert org ID. Sessions for different orgs on the
// same endpoint are distinct and must not be collapsed to a single picker row.
func authSessionKey(a *config.AuthConfig) string {
	if len(a.Certificates) > 0 {
		return fmt.Sprintf("%s::%s", a.CloudGRPC, a.OrganizationKey())
	}
	if a.OAuthIssuer != "" {
		return fmt.Sprintf("%s::%s", a.CloudGRPC, a.OAuthIssuer)
	}
	return a.CloudGRPC
}

var authPickerColumns = []tui.PickerColumn{
	{
		Title:    "Name",
		MinWidth: 20,
		Required: true,
		Value:    func(item tui.PickerItem) string { return item.Name },
	},
	{
		Title:    "Org. ID",
		MinWidth: 8,
		Value:    func(item tui.PickerItem) string { return item.Description },
	},
	{
		Title:    "Environment",
		MinWidth: 16,
		Value:    func(item tui.PickerItem) string { return item.Type },
	},
}

// resolveAuthOrgNames refreshes names for both UUID and legacy sessions. Keys
// include the endpoint so equal IDs in different environments stay distinct.
func resolveAuthOrgNames(ctx context.Context, cfg *config.Config) map[string]string {
	names := make(map[string]string)
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		key := authSessionKey(a)
		if names[key] == "" {
			names[key] = cloudOrganizationName(ctx, a)
		}
	}
	return names
}

// authPickerItems builds picker rows for every stored session.
// orgNames is a pre-fetched map of session key -> name; missing entries fall back to
// "org N". The Name column shows the org name, Description shows the org ID,
// and Type carries the environment (dashboard URL or gRPC endpoint).
// DedupKey and Value carry the session key so each (endpoint, org) pair is a
// distinct row even when multiple orgs share the same gRPC endpoint.
func authPickerItems(cfg *config.Config, orgNames map[string]string) []tui.PickerItem {
	// A legacy login and its operator/OIDC replacement can share the same
	// endpoint and certificate org ID. They intentionally have the same picker
	// key, so retain the operator-capable row rather than letting the stale
	// legacy row win by insertion order.
	preferred := make(map[string]*config.AuthConfig, len(cfg.Auth))
	order := make([]string, 0, len(cfg.Auth))
	for i := range cfg.Auth {
		a := &cfg.Auth[i]
		key := authSessionKey(a)
		if _, exists := preferred[key]; !exists {
			order = append(order, key)
		}
		if current := preferred[key]; current == nil || (current.OAuthIssuer == "" && a.OAuthIssuer != "") {
			preferred[key] = a
		}
	}

	items := make([]tui.PickerItem, 0, len(preferred))
	for _, key := range order {
		a := preferred[key]
		name := a.CloudGRPC
		idStr := ""
		if len(a.Certificates) > 0 && a.Certificates[0].TenantUUID() != "" {
			idStr = a.Certificates[0].TenantUUID()
			name = idStr
		} else if len(a.Certificates) > 0 {
			orgID := int32(a.Certificates[0].OrganizationID)
			idStr = fmt.Sprintf("%d", orgID)
			if n, ok := orgNames[key]; ok && n != "" {
				name = n
			} else {
				name = fmt.Sprintf("org %d", orgID)
			}
		} else if a.OAuthIssuer != "" {
			name = issuerRealm(a.OAuthIssuer)
		}
		if resolved := orgNames[key]; resolved != "" {
			name = resolved
		} else if cached := cachedCloudOrganizationName(a); cached != "" {
			name = cached
		}

		env := a.CloudDashboard
		if env == "" {
			env = a.CloudGRPC
		}

		items = append(items, tui.PickerItem{
			Name:        name,
			Description: idStr,
			Type:        env,
			DedupKey:    key,
			Value:       key,
		})
	}
	return items
}

// persistSessionDefault stores the picker's highlighted session ("endpoint" or
// "endpoint::orgID" key) as the default. Both halves are persisted: the org ID
// is what actually disambiguates sessions when several orgs share one endpoint
// — persisting only the endpoint made the default resolve to whichever of them
// was logged into first, not the one the user picked.
func persistSessionDefault(key string) error {
	c, err := config.Load()
	if err != nil {
		return err
	}
	endpoint := key
	orgID := 0
	tenant := ""
	if idx := strings.Index(key, "::"); idx >= 0 {
		endpoint = key[:idx]
		if n, convErr := strconv.Atoi(key[idx+2:]); convErr == nil {
			orgID = n
		} else if _, err := uuid.Parse(key[idx+2:]); err == nil {
			tenant = key[idx+2:]
		} else {
			return fmt.Errorf("invalid organization identifier")
		}

	}
	c.DefaultCloudGRPC = endpoint
	c.DefaultOrgID = int32(orgID)
	c.DefaultTenantUUID = tenant
	return config.Save(c)
}

// pickAuthSession shows the interactive session picker. 'd' marks the
// highlighted session as the persisted default (written immediately, mirroring
// the device picker), 'x' clears it, and Enter selects a session for this
// invocation only. Returns the selected session (cert-validated).
func pickAuthSession(cfg *config.Config) (*config.AuthConfig, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	picker := tui.NewPickerWithTitleAndColumns("Select an organisation", authPickerColumns)
	// Compute the default key from the stored default org ID (preferred) or
	// the legacy DefaultCloudGRPC field so both code-paths work.
	if cfg.DefaultTenantUUID != "" && cfg.DefaultCloudGRPC != "" {
		picker.DefaultKey = strings.ToLower(cfg.DefaultCloudGRPC + "::" + cfg.DefaultTenantUUID)
	}
	if cfg.DefaultOrgID != 0 {
		for i := range cfg.Auth {
			if key := authSessionKey(&cfg.Auth[i]); strings.HasSuffix(key, fmt.Sprintf("::%d", cfg.DefaultOrgID)) {
				picker.DefaultKey = strings.ToLower(key)
				break
			}
		}
	}
	if picker.DefaultKey == "" && cfg.DefaultCloudGRPC != "" {
		for i := range cfg.Auth {
			if cfg.Auth[i].CloudGRPC == cfg.DefaultCloudGRPC {
				picker.DefaultKey = strings.ToLower(authSessionKey(&cfg.Auth[i]))
				break
			}
		}
	}

	picker.OnSetDefault = func(item tui.PickerItem) string {
		key, _ := item.Value.(string)
		if key == "" {
			return ""
		}
		if err := persistSessionDefault(key); err != nil {
			return fmt.Sprintf("Could not save default: %v", err)
		}
		return fmt.Sprintf("Default set to %s.", item.Name)
	}
	picker.OnUnsetDefault = func() string {
		if c, err := config.Load(); err == nil {
			c.DefaultCloudGRPC = ""
			c.DefaultOrgID = 0
			c.DefaultTenantUUID = ""
			_ = config.Save(c)
		}
		return "Default cleared."
	}

	// Snapshot the rows before background lookups; the caller can use its
	// selected session as soon as the picker exits.
	lookupCfg := *cfg
	lookupCfg.Auth = append([]config.AuthConfig(nil), cfg.Auth...)
	for i := range lookupCfg.Auth {
		lookupCfg.Auth[i].Certificates = append([]config.CertificateInfo(nil), cfg.Auth[i].Certificates...)
	}
	p := tea.NewProgram(picker)
	go func() {
		p.Send(tui.PickerAddMsg{Items: authPickerItems(&lookupCfg, nil)})
		p.Send(tui.PickerSetMsg{Items: authPickerItems(&lookupCfg, resolveAuthOrgNames(ctx, &lookupCfg))})
		p.Send(tui.PickerDoneMsg{})
	}()

	finalModel, err := p.Run()
	if err != nil {
		return nil, fmt.Errorf("picker: %w", err)
	}
	pm := finalModel.(tui.PickerModel)
	if pm.Cancelled() {
		return nil, ErrUserCancelled
	}
	if pm.Selected() == nil {
		return nil, fmt.Errorf("no organisation selected")
	}
	selectedKey := pm.Selected().Value.(string)
	var selected *config.AuthConfig
	for i := range cfg.Auth {
		if authSessionKey(&cfg.Auth[i]) == selectedKey {
			candidate := &cfg.Auth[i]
			if selected == nil || (selected.OAuthIssuer == "" && candidate.OAuthIssuer != "") {
				selected = candidate
			}
		}
	}
	if selected != nil {
		if len(selected.Certificates) == 0 && !selected.HasAPIKey() {
			return nil, fmt.Errorf("auth session has no certificates or API token; re-run 'wendy auth login'")
		}
		return selected, nil
	}
	return nil, fmt.Errorf("selected session no longer exists")
}

// pickAuthSessionFn is the indirection point so tests can stub the picker.
var pickAuthSessionFn = pickAuthSession
