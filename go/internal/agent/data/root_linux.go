//go:build linux

package data

import "golang.org/x/sys/unix"

// WendyOS keeps its large persistent partition at /data. Do not select an
// ordinary /data directory on the OS filesystem if that partition is absent.
func defaultDataRoot() (string, error) {
	var system, persistent unix.Stat_t
	if unix.Stat("/", &system) != nil || unix.Stat("/data", &persistent) != nil || system.Dev == persistent.Dev {
		return DefaultRoot, nil
	}
	return migrateDataRoot(DefaultRoot, "/data/wendy-agent/data/episodes")
}
