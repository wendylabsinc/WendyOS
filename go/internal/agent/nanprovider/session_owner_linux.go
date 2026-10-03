//go:build linux

package nanprovider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

const (
	sessionOwnerPath = "/run/wendy-nan-provider.owner"
	nanControlPath   = "/run/wpa_supplicant/nan0"
)

// A NAN management interface is an nl80211 wdev, not an rtnetlink netdev.
// Its supplicant control socket is removed and recreated by the helper's
// interface_remove/interface_add lifecycle, even if supplicant itself stays up.
type socketIdentity struct {
	dev, ino            uint64
	ctimeSec, ctimeNsec int64
}

func nanControlSocketIdentity() (socketIdentity, error) {
	return controlSocketIdentity(nanControlPath)
}

func controlSocketIdentity(path string) (socketIdentity, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return socketIdentity{}, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return socketIdentity{}, fmt.Errorf("NAN control path %s is not a Unix socket", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return socketIdentity{}, fmt.Errorf("NAN control path %s has unsupported stat metadata", path)
	}
	return socketIdentity{dev: uint64(stat.Dev), ino: stat.Ino, ctimeSec: stat.Ctim.Sec, ctimeNsec: stat.Ctim.Nsec}, nil
}

type sessionPlan struct {
	own, reset bool
}

func planSession(started, reclaimedNDI, marked bool) sessionPlan {
	own := !started || reclaimedNDI || marked
	return sessionPlan{own: own, reset: started && own}
}

func ownerContents(id Identity, socket socketIdentity) string {
	return fmt.Sprintf("v1:%d:%d:%d:%d:%d:%d\n", id.Org, id.Asset, socket.dev, socket.ino, socket.ctimeSec, socket.ctimeNsec)
}

func sessionOwnerMatches(id Identity, socket socketIdentity) (bool, error) {
	return ownerMatches(sessionOwnerPath, id, socket)
}

func ownerMatches(path string, id Identity, socket socketIdentity) (bool, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	recorded, recordedSocket, err := parseOwner(path, contents)
	if err != nil {
		return false, err
	}
	if recorded != id {
		return false, fmt.Errorf("NAN ownership marker %s belongs to org %d asset %d", path, recorded.Org, recorded.Asset)
	}
	// A zero token records START in progress. After START, the marker is pinned
	// to the socket incarnation so a later host-shell stop/start cannot inherit it.
	return recordedSocket == (socketIdentity{}) || recordedSocket == socket, nil
}

func parseOwner(path string, contents []byte) (Identity, socketIdentity, error) {
	var id Identity
	var socket socketIdentity
	if _, err := fmt.Sscanf(string(contents), "v1:%d:%d:%d:%d:%d:%d\n", &id.Org, &id.Asset,
		&socket.dev, &socket.ino, &socket.ctimeSec, &socket.ctimeNsec); err != nil ||
		string(contents) != ownerContents(id, socket) {
		return Identity{}, socketIdentity{}, fmt.Errorf("invalid NAN ownership marker %s", path)
	}
	return id, socket, nil
}

func markSessionOwner(id Identity, socket socketIdentity) error {
	return markOwner(sessionOwnerPath, id, socket)
}

func markOwner(path string, id Identity, socket socketIdentity) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".wendy-nan-owner-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.WriteString(ownerContents(id, socket)); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func clearSessionOwner(id Identity) error {
	return clearOwner(sessionOwnerPath, id)
}

func clearOwner(path string, id Identity) error {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	recorded, _, err := parseOwner(path, contents)
	if err != nil {
		return err
	}
	if recorded != id {
		return fmt.Errorf("NAN ownership marker %s belongs to org %d asset %d", path, recorded.Org, recorded.Asset)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
