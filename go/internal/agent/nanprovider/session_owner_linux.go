//go:build linux

package nanprovider

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/agent/nancontrol"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
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
	token, err := nancontrol.SocketIdentity(path)
	if err != nil {
		return socketIdentity{}, err
	}
	return socketIdentity{dev: token.Dev, ino: token.Ino, ctimeSec: token.CtimeSec, ctimeNsec: token.CtimeNsec}, nil
}

type sessionPlan struct {
	own, reset bool
}

// Only absence or a matching management-interface marker grants ownership.
// A private data interface is independent of whoever created nan0.
func planSession(present, marked bool) sessionPlan {
	own := !present || marked
	return sessionPlan{own: own, reset: present && own}
}

func ownerContents(id meshid.Device, socket socketIdentity) string {
	return fmt.Sprintf("v2 %s %d %d %d %d\n", id.Principal(), socket.dev, socket.ino, socket.ctimeSec, socket.ctimeNsec)
}

func sessionOwnerMatches(id meshid.Device, socket socketIdentity) (bool, error) {
	return ownerMatches(sessionOwnerPath, id, socket)
}

func ownerMatches(path string, id meshid.Device, socket socketIdentity) (bool, error) {
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
		return false, fmt.Errorf("NAN ownership marker %s belongs to %s", path, recorded.Principal())
	}
	// A zero token records START in progress. After START, the marker is pinned
	// to the socket incarnation so a later host-shell stop/start cannot inherit it.
	return recordedSocket == (socketIdentity{}) || recordedSocket == socket, nil
}

func parseOwner(path string, contents []byte) (meshid.Device, socketIdentity, error) {
	fields := strings.Split(strings.TrimSuffix(string(contents), "\n"), " ")
	if len(fields) != 6 || fields[0] != "v2" {
		return meshid.Device{}, socketIdentity{}, fmt.Errorf("invalid NAN ownership marker %s", path)
	}
	id, err := meshid.DeviceFromPrincipal(fields[1])
	if err != nil {
		return meshid.Device{}, socketIdentity{}, fmt.Errorf("invalid NAN ownership marker %s", path)
	}
	socket := socketIdentity{}
	if socket.dev, err = strconv.ParseUint(fields[2], 10, 64); err != nil {
		return meshid.Device{}, socketIdentity{}, fmt.Errorf("invalid NAN ownership marker %s", path)
	}
	if socket.ino, err = strconv.ParseUint(fields[3], 10, 64); err != nil {
		return meshid.Device{}, socketIdentity{}, fmt.Errorf("invalid NAN ownership marker %s", path)
	}
	if socket.ctimeSec, err = strconv.ParseInt(fields[4], 10, 64); err != nil {
		return meshid.Device{}, socketIdentity{}, fmt.Errorf("invalid NAN ownership marker %s", path)
	}
	if socket.ctimeNsec, err = strconv.ParseInt(fields[5], 10, 64); err != nil {
		return meshid.Device{}, socketIdentity{}, fmt.Errorf("invalid NAN ownership marker %s", path)
	}
	if string(contents) != ownerContents(id, socket) {
		return meshid.Device{}, socketIdentity{}, fmt.Errorf("invalid NAN ownership marker %s", path)
	}
	return id, socket, nil
}

func markSessionOwner(id meshid.Device, socket socketIdentity) error {
	return markOwner(sessionOwnerPath, id, socket)
}

func markOwner(path string, id meshid.Device, socket socketIdentity) error {
	return writeOwnerFile(path, ownerContents(id, socket))
}

func writeOwnerFile(path, contents string) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".wendy-nan-owner-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.WriteString(contents); err != nil {
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

func clearSessionOwner(id meshid.Device) error {
	return clearOwner(sessionOwnerPath, id)
}

func clearOwner(path string, id meshid.Device) error {
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
		return fmt.Errorf("NAN ownership marker %s belongs to %s", path, recorded.Principal())
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
