package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// Organization names are display hints, stored separately from credentials. Keep
// stale names for offline use; every successful lookup refreshes them. Endpoint
// and organization identity both scope the cache, including for legacy IDs.
type organizationNameCache struct {
	Version int                          `json:"version"`
	Names   map[string]map[string]string `json:"names"`
}

var organizationNameCacheMu sync.Mutex

func readOrganizationNameCache(path string) organizationNameCache {
	var cache organizationNameCache
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &cache) != nil || cache.Version != 1 || cache.Names == nil {
		return organizationNameCache{Version: 1, Names: make(map[string]map[string]string)}
	}
	return cache
}

func loadOrganizationNameCache() (organizationNameCache, string, bool) {
	dir, err := ConfigDir()
	if err != nil {
		return organizationNameCache{}, "", false
	}
	path := filepath.Join(dir, "organization-names.json")
	return readOrganizationNameCache(path), path, true
}

// OrganizationName returns the last known name of an organization on a Cloud
// endpoint, or "" when this machine has never seen it.
func OrganizationName(endpoint, organization string) string {
	organizationNameCacheMu.Lock()
	defer organizationNameCacheMu.Unlock()
	cache, _, ok := loadOrganizationNameCache()
	if !ok {
		return ""
	}
	return cache.Names[endpoint][organization]
}

// SetOrganizationName records an organization's name for later display.
func SetOrganizationName(endpoint, organization, name string) {
	if endpoint == "" || organization == "" || strings.TrimSpace(name) == "" {
		return
	}
	organizationNameCacheMu.Lock()
	defer organizationNameCacheMu.Unlock()
	cache, path, ok := loadOrganizationNameCache()
	if !ok {
		return
	}
	if cache.Names[endpoint] == nil {
		cache.Names[endpoint] = make(map[string]string)
	}
	cache.Names[endpoint][organization] = name
	data, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".organization-names-*.json")
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

// OrganizationLabel renders an organization the way people know it, name first
// with the ID secondary (`"Robotics" (7)`), or the bare ID when no name is known
// (WDY-3797). The name is an organization's own free text, so it is quoted and
// escaped: quotes, newlines and terminal controls cannot forge another label.
// It reads only the local name cache and never calls Cloud, so it is safe on
// error paths and offline, and it can only show names this machine has already
// been told. An empty endpoint matches the organization on any endpoint, but
// only when every endpoint that knows it agrees on the name: legacy numeric IDs
// repeat across clouds.
func OrganizationLabel(endpoint, organization string) string {
	if organization == "" {
		return ""
	}
	organizationNameCacheMu.Lock()
	defer organizationNameCacheMu.Unlock()
	cache, _, ok := loadOrganizationNameCache()
	if !ok {
		return organization
	}
	name := ""
	if endpoint != "" {
		name = cache.Names[endpoint][organization]
	} else {
		for _, names := range cache.Names {
			candidate := names[organization]
			if candidate == "" {
				continue
			}
			if name != "" && name != candidate {
				return organization
			}
			name = candidate
		}
	}
	if name == "" {
		return organization
	}
	return strconv.Quote(name) + " (" + organization + ")"
}
