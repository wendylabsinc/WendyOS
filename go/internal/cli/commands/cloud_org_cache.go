package commands

import (
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// The organization name cache lives in config so packages outside commands
// can label organizations too; these keep the commands-side names.

func cachedCloudOrganizationName(auth *config.AuthConfig) string {
	if auth == nil || len(auth.Certificates) == 0 {
		return ""
	}
	return cachedOrganizationName(auth.CloudGRPC, auth.OrganizationKey())
}

func cachedOrganizationName(endpoint, organization string) string {
	return config.OrganizationName(endpoint, organization)
}

func cacheCloudOrganizationName(endpoint, organization, name string) {
	config.SetOrganizationName(endpoint, organization, name)
}

// orgLabel renders a legacy numeric organization as `"Name" (7)`, or "7" when
// its name is unknown. An empty endpoint matches any cloud; see
// config.OrganizationLabel.
func orgLabel(endpoint string, id int32) string {
	return config.OrganizationLabel(endpoint, strconv.FormatInt(int64(id), 10))
}
