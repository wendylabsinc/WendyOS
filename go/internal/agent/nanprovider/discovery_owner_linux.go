//go:build linux

package nanprovider

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const (
	discoveryOwnerPath = "/run/wendy-nan-provider.discovery"
	nanServiceName     = "wendy.mesh.v1" // Existing discovery wire contract.
)

// The provider lock protects this journal. Discovery handles outlive a crashed
// control client even when someone else owns nan0. Record each returned handle
// against its management-socket incarnation; never infer ownership from a
// service name alone or restart borrowed management to remove our services.
type discoveryLease struct {
	control      radioCommander
	path         string
	device       meshid.Device
	socket       socketIdentity
	publish, sub string
}

func (d *discoveryLease) contents() string {
	return ownerContents(d.device, d.socket) + fmt.Sprintf("publish=%s subscribe=%s\n", d.publish, d.sub)
}

func readDiscoveryLease(path string, control radioCommander) (*discoveryLease, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(b), "\n")
	if len(lines) != 3 || lines[2] != "" {
		return nil, fmt.Errorf("invalid NAN discovery journal %s", path)
	}
	id, socket, err := parseOwner(path, []byte(lines[0]+"\n"))
	if err != nil || socket == (socketIdentity{}) {
		return nil, fmt.Errorf("invalid NAN discovery socket in %s", path)
	}
	v := fields(lines[1])
	d := &discoveryLease{control: control, path: path, device: id, socket: socket, publish: v["publish"], sub: v["subscribe"]}
	if (d.publish != "" && !validHandle(d.publish)) || (d.sub != "" && !validHandle(d.sub)) ||
		(d.publish != "" && d.publish == d.sub) || d.contents() != string(b) {
		return nil, fmt.Errorf("invalid NAN discovery handles in %s", path)
	}
	return d, nil
}

type discoveryService struct{ kind, name string }

// Require a complete, unambiguous inventory before acting on persisted IDs.
// A reused handle with a different name/type belongs to another client.
func discoveryServices(control radioCommander) (map[string]discoveryService, error) {
	status, err := control.command("NAN_STATUS")
	if err != nil {
		return nil, err
	}
	services := make(map[string]discoveryService)
	count := -1
	for _, line := range strings.Split(status, "\n") {
		v := fields(line)
		if raw, ok := v["num_services"]; ok {
			if count >= 0 {
				return nil, errors.New("duplicate NAN discovery inventory")
			}
			count, err = strconv.Atoi(raw)
			if err != nil || count < 0 {
				return nil, errors.New("invalid NAN discovery inventory count")
			}
		}
		if handle, ok := v["service"]; ok {
			if !validHandle(handle) || v["type"] == "" {
				return nil, errors.New("invalid NAN discovery service")
			}
			if _, duplicate := services[handle]; duplicate {
				return nil, errors.New("duplicate NAN discovery handle")
			}
			services[handle] = discoveryService{kind: v["type"], name: v["name"]}
		}
	}
	if count != len(services) {
		return nil, errors.New("incomplete NAN discovery inventory")
	}
	return services, nil
}

func (d *discoveryLease) release() error {
	services, err := discoveryServices(d.control)
	if err != nil {
		return err
	}
	for _, entry := range []struct{ handle, kind string }{{d.sub, "subscribe"}, {d.publish, "publish"}} {
		service, exists := services[entry.handle]
		if !exists || service.name != nanServiceName || service.kind != entry.kind {
			continue
		}
		command := "NAN_CANCEL_PUBLISH publish_id="
		if entry.kind == "subscribe" {
			command = "NAN_CANCEL_SUBSCRIBE subscribe_id="
		}
		if _, err := d.control.command(command + entry.handle); err != nil {
			return err // Keep the journal so a later run can finish cleanup.
		}
	}
	// A failed persistence attempt can leave an earlier version of our journal.
	// Preserve it (and any administrator edit); next startup verifies it again.
	if b, err := os.ReadFile(d.path); err == nil && string(b) == d.contents() {
		return os.Remove(d.path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func startDiscovery(control radioCommander, path string, id meshid.Device, socket socketIdentity, ssi string) (lease *discoveryLease, err error) {
	previous, err := readDiscoveryLease(path, control)
	if err == nil {
		if previous.device != id {
			return nil, errors.New("NAN discovery journal belongs to another device")
		}
		if previous.socket == socket {
			if err := previous.release(); err != nil {
				return nil, fmt.Errorf("reclaim NAN discovery: %w", err)
			}
		} else if err := os.Remove(path); err != nil {
			return nil, err // New socket: old handle numbers grant no authority.
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	services, err := discoveryServices(control)
	if err != nil {
		return nil, err
	}
	for _, service := range services {
		if service.name == nanServiceName {
			return nil, errors.New("NAN wendy net discovery is already in use without matching ownership")
		}
	}
	d := &discoveryLease{control: control, path: path, device: id, socket: socket}
	if err := writeOwnerFile(path, d.contents()); err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, d.release())
		}
	}()
	d.publish, err = control.command("NAN_PUBLISH service_name=" + nanServiceName + " sync=1 data_path=1 ttl=0 ssi=" + ssi)
	if err != nil {
		return nil, err
	}
	if !validHandle(d.publish) {
		return nil, fmt.Errorf("invalid NAN publish handle %q", d.publish)
	}
	if err = writeOwnerFile(path, d.contents()); err != nil {
		return nil, err
	}
	d.sub, err = control.command("NAN_SUBSCRIBE service_name=" + nanServiceName + " sync=1 ttl=0 ssi=" + ssi)
	if err != nil {
		return nil, err
	}
	if !validHandle(d.sub) || d.sub == d.publish {
		return nil, fmt.Errorf("invalid NAN subscribe handle %q", d.sub)
	}
	if err = writeOwnerFile(path, d.contents()); err != nil {
		return nil, err
	}
	return d, nil
}
