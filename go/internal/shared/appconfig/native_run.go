package appconfig

import (
	"fmt"
	"path"
	"strings"
)

func (c *AppConfig) validateNativeRun() error {
	r := c.Run
	if r.Cwd != "" && !SafeNativeRelativePath(r.Cwd, true) {
		return fmt.Errorf("run.cwd must be a relative directory inside the app directory")
	}
	if r.Command == "" {
		return nil
	}
	if strings.ContainsAny(r.Command, "\x00\r\n\\") || (!path.IsAbs(r.Command) && !SafeNativeRelativePath(r.Command, false)) {
		return fmt.Errorf("run.command must name an absolute target executable or a synced executable inside the app directory")
	}
	if len(c.Services) != 0 {
		return fmt.Errorf("run.command supports only a single native Darwin app")
	}
	if c.Platform != "" && c.Platform != PlatformDarwin && !strings.HasPrefix(c.Platform, PlatformDarwin+"/") {
		return fmt.Errorf("run.command requires a Darwin target")
	}
	if c.Xcode != nil {
		return fmt.Errorf("run.command cannot be combined with xcode build configuration")
	}
	return nil
}

func SafeNativeRelativePath(value string, allowCurrent bool) bool {
	if value == "" || path.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." {
			return false
		}
	}
	return allowCurrent || path.Clean(value) != "."
}
