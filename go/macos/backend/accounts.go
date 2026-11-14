package backend

import (
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Organization is local identity metadata, not an authorization decision.
// Cloud still validates the certificate and access on every request.
type SavedAccountInfo struct {
	commands.DesktopAccountInfo
	OrganizationID, OrganizationName string
}

func savedAccountInfos() ([]SavedAccountInfo, error) {
	entries, err := commands.DesktopAccounts()
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}
	result := make([]SavedAccountInfo, 0, len(entries))
	for _, entry := range entries {
		item := SavedAccountInfo{DesktopAccountInfo: entry}
		if auth, ok := cfg.ContextByName(entry.Context); ok && len(auth.Certificates) > 0 {
			item.OrganizationID = auth.Certificates[0].TenantUUID()
			item.OrganizationName = config.OrganizationName(auth.CloudGRPC, item.OrganizationID)
		}
		result = append(result, item)
	}
	return result, nil
}
