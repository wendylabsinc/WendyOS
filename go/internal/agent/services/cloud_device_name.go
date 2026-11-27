package services

import (
	"encoding/json"
	"os"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/shared/atomicfile"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshname"
)

type registeredDeviceName struct {
	Principal string `json:"principal"`
	Name      string `json:"name"`
}

// UseLocalNameCache loads the last Cloud-confirmed name for offline boot.
func (r *MeshRoster) UseLocalNameCache(configDir string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.localNameFile = filepath.Join(configDir, "cloud-device-name.json")
	r.restoreLocalName()
}

// LocalCloudName returns the name Cloud confirmed for this exact device.
// An empty result leaves the device reachable by UUID.
func (r *MeshRoster) LocalCloudName(device meshid.Device) string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !device.IsValid() || r.localName.Principal != device.Principal() {
		return ""
	}
	return r.localName.Name
}

// restoreLocalName and rememberLocalName run with the roster write lock held.
func (r *MeshRoster) restoreLocalName() {
	r.localName = registeredDeviceName{}
	r.nameNeedsSave = false
	if r.identity == "" || r.localNameFile == "" {
		return
	}
	data, err := os.ReadFile(r.localNameFile)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		r.logger.Warn("wendy net: reading cached Cloud device name failed", zap.Error(err))
		return
	}
	var saved registeredDeviceName
	if err := json.Unmarshal(data, &saved); err != nil || saved.Principal != r.identity {
		return
	}
	if saved.Name != "" {
		if label, valid := meshname.FriendlyLabel(saved.Name); !valid || label != saved.Name {
			return
		}
	}
	r.localName = saved
}

func (r *MeshRoster) rememberLocalName(name registeredDeviceName) {
	if r.localName != name {
		r.localName = name
		r.nameNeedsSave = r.localNameFile != ""
	}
	if !r.nameNeedsSave {
		return
	}
	data, err := json.Marshal(name)
	if err == nil {
		err = atomicfile.Write(r.localNameFile, data, 0o600)
	}
	if err != nil {
		r.logger.Warn("wendy net: saving Cloud device name for offline use failed", zap.Error(err))
		return
	}
	r.nameNeedsSave = false
}
