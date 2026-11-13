//go:build linux

package commands

import (
	"fmt"
	"os"
	"os/user"
	"strconv"
	"syscall"
)

func serialPortGroup(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ""
	}
	group, err := user.LookupGroupId(strconv.Itoa(int(stat.Gid)))
	if err != nil {
		return strconv.Itoa(int(stat.Gid))
	}
	return group.Name
}

// portPermissionError explains a permission denied on portPath, owned by group.
func portPermissionError(portPath, group string, openErr error) error {
	cur, err := user.Current()
	if err != nil {
		return permissionMessage(portPath, group, "", false, false, openErr)
	}
	listed := false
	if ids, err := cur.GroupIds(); err == nil {
		if g, err := user.LookupGroup(group); err == nil {
			for _, id := range ids {
				listed = listed || id == g.Gid
			}
		}
	}
	active := false
	if gids, err := os.Getgroups(); err == nil {
		if g, err := user.LookupGroup(group); err == nil {
			for _, id := range gids {
				active = active || strconv.Itoa(id) == g.Gid
			}
		}
	}
	return permissionMessage(portPath, group, cur.Username, active, listed, openErr)
}

// permissionMessage picks the true reason for a denied port: the process has
// the group (active), the database lists the user but the session predates it
// (listed), or the user is not in the group.
func permissionMessage(portPath, group, username string, active, listed bool, openErr error) error {
	switch {
	case active:
		return fmt.Errorf("Permission denied opening %s although this session has group '%s', so something else blocks the port: %w", portPath, group, openErr)
	case listed:
		return fmt.Errorf("Permission denied opening %s: you are in group '%s', but this login session started before that. Log out and back in, or run `newgrp %s` in this terminal, then try again.", portPath, group, group)
	default:
		return fmt.Errorf("Permission denied opening %s: you are not in group '%s'. Run `sudo usermod -aG %s %s`, then log out and back in.", portPath, group, group, username)
	}
}
