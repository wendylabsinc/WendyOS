package containerd

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
)

const worldGenerationLabel = "sh.wendy/world.deployment-generation"
const worldDirectoryLabel = "sh.wendy/world.socket-directory"

// A provider must compare all immutable scope fields. ValidateMount never
// creates authority or a listener; an agent restart may require redeployment.
type AppWorldSocketProvider interface {
	EnsureAccess(appID, service string, generation uint64, publish []appconfig.WorldPublication, subscribe []appconfig.WorldSubscription) (string, error)
	ValidateAccessMount(appID, service string, generation uint64, directory string, publish []appconfig.WorldPublication, subscribe []appconfig.WorldSubscription) error
	Release(appID, service string, generation uint64)
	ReleaseApp(appID string)
}

func (c *Client) SetAppWorldSocketProvider(provider AppWorldSocketProvider) {
	c.worldSocketProvider = provider
}

func worldGeneration() (uint64, error) {
	for {
		var data [8]byte
		if _, err := rand.Read(data[:]); err != nil {
			return 0, err
		}
		if n := binary.BigEndian.Uint64(data[:]); n != 0 {
			return n, nil
		}
	}
}

func worldGenerationFromLabels(labels map[string]string) (uint64, error) {
	raw := labels[worldGenerationLabel]
	generation, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || generation == 0 || strconv.FormatUint(generation, 10) != raw {
		return 0, fmt.Errorf("invalid persisted world deployment generation; redeploy required")
	}
	return generation, nil
}

func (c *Client) validateWorldMount(labels map[string]string) error {
	publish, subscribe, found, err := appconfig.WorldAccess(parseEntitlementsFromAnnotations(labels))
	if err != nil {
		return err
	}
	if !found {
		if labels[worldGenerationLabel] != "" || labels[worldDirectoryLabel] != "" {
			return fmt.Errorf("world mount labels without entitlement; redeploy required")
		}
		return nil
	}
	if c.worldSocketProvider == nil {
		return fmt.Errorf("world publication provider unavailable; redeploy required")
	}
	app, service := labels[labelKeyAppID], labels[labelKeyServiceName]
	generation, err := worldGenerationFromLabels(labels)
	if err != nil {
		return err
	}
	if _, err = appconfig.WorldCgroupSuffix(app, service, generation); err != nil {
		return err
	}
	directory := labels[worldDirectoryLabel]
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return fmt.Errorf("invalid persisted world directory; redeploy required")
	}
	if err = c.worldSocketProvider.ValidateAccessMount(app, service, generation, directory, publish, subscribe); err != nil {
		return fmt.Errorf("world mount unavailable; redeploy required: %w", err)
	}
	return nil
}
