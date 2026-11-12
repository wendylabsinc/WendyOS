package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

// BuildID identifies the running OS image, read only from the rootfs so a
// /data copy can't make two images look alike. "" means unknown.
func BuildID(root string) string {
	h := sha256.New()
	for _, rel := range []string{"usr/lib/wendyos/version.txt", "usr/lib/wendyos/commit", "opt/wendyos/bin/wendy-agent"} {
		f, err := os.Open(filepath.Join(root, rel))
		if err != nil {
			return ""
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return ""
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}
