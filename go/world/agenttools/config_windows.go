package agenttools

import "github.com/wendylabsinc/wendy/go/world/internal/client"

// Windows profiles need an ACL-aware loader before they can hold signing keys.
func loadConfig(string) (client.Config, error) { return client.Config{}, ErrUnavailable }
