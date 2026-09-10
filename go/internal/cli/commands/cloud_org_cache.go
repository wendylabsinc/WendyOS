package commands

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// Organization names are display hints, stored separately from credentials. Keep
// stale names for offline use; every successful lookup refreshes them. Endpoint
// and organization identity both scope the cache, including for legacy IDs.
type cloudOrgNameCache struct {
	Version int                          `json:"version"`
	Names   map[string]map[string]string `json:"names"`
}

var cloudOrgNameCacheMu sync.Mutex

func readCloudOrgNameCache(path string) cloudOrgNameCache {
	var cache cloudOrgNameCache
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &cache) != nil || cache.Version != 1 || cache.Names == nil {
		return cloudOrgNameCache{Version: 1, Names: make(map[string]map[string]string)}
	}
	return cache
}

func cachedCloudOrganizationName(auth *config.AuthConfig) string {
	if auth == nil || len(auth.Certificates) == 0 {
		return ""
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return ""
	}
	cloudOrgNameCacheMu.Lock()
	defer cloudOrgNameCacheMu.Unlock()
	return readCloudOrgNameCache(filepath.Join(dir, "organization-names.json")).Names[auth.CloudGRPC][auth.OrganizationKey()]
}

func cacheCloudOrganizationName(endpoint, organization, name string) {
	if endpoint == "" || organization == "" || strings.TrimSpace(name) == "" {
		return
	}
	dir, err := config.ConfigDir()
	if err != nil {
		return
	}
	cloudOrgNameCacheMu.Lock()
	defer cloudOrgNameCacheMu.Unlock()
	path := filepath.Join(dir, "organization-names.json")
	cache := readCloudOrgNameCache(path)
	if cache.Names[endpoint] == nil {
		cache.Names[endpoint] = make(map[string]string)
	}
	cache.Names[endpoint][organization] = name
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	file, err := os.CreateTemp(dir, ".organization-names-*.json")
	if err != nil {
		return
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr == nil && closeErr == nil {
		_ = os.Rename(file.Name(), path)
	}
}
