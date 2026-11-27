package oci

import (
	"fmt"
	"os"
	"path/filepath"
)

func ValidateWorldSocketDirectory(directory string) error {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return fmt.Errorf("world socket directory must be absolute and canonical")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("world socket directory unavailable")
	}
	info, err = os.Lstat(filepath.Join(directory, "world.sock"))
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("world socket unavailable")
	}
	return nil
}

func applyWorldSocket(spec *Spec, directory string) {
	if ValidateWorldSocketDirectory(directory) != nil {
		return
	}
	spec.Mounts = append(spec.Mounts, Mount{Destination: "/run/wendy/world", Source: directory, Type: "bind", Options: []string{"rbind", "nosuid", "noexec", "ro"}})
	spec.Process.User.AdditionalGids = appendUnique(spec.Process.User.AdditionalGids, appSystemAPIGroupGID)
	spec.Process.Env = append(spec.Process.Env, "WENDY_WORLD_SOCKET=/run/wendy/world/world.sock")
}
