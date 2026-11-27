//go:build darwin || linux || windows

package commands

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// A cached OS image is used only when it is the image the manifest publishes,
// wherever the manifest publishes a checksum for it. Hashing a multi-gigabyte
// file on every install is too slow, so a file that passed gets a stamp beside
// it, as an extracted flashpack does (flashpack.StampPath): the next install
// compares the stamp with the manifest instead of reading the file again.
//
// That also covers a build republished under an unchanged version, which is
// what every push to a PR does to its "pr-N" tag: the stamp then names the
// earlier checksum, so the cached build is replaced rather than flashed.

// imageStamp is what the stamp beside a cached image records: the manifest
// checksum the file matched, and the size and modification time of the file
// that was hashed. The last two tie the stamp to that file, so one that was
// cut short or replaced since is hashed again instead of being trusted.
type imageStamp struct {
	checksum string
	size     int64
	mtimeNs  int64
}

func imageStampPath(cached string) string { return cached + ".sha256" }

// readImageStamp returns the stamp of the cached image, or false when there
// is none that can be read: a file an older wendy cached has no stamp.
func readImageStamp(cached string) (imageStamp, bool) {
	data, err := os.ReadFile(imageStampPath(cached))
	if err != nil {
		return imageStamp{}, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 3 {
		return imageStamp{}, false
	}
	size, sizeErr := strconv.ParseInt(fields[1], 10, 64)
	mtimeNs, mtimeErr := strconv.ParseInt(fields[2], 10, 64)
	if sizeErr != nil || mtimeErr != nil {
		return imageStamp{}, false
	}
	return imageStamp{checksum: fields[0], size: size, mtimeNs: mtimeNs}, true
}

// stampCachedImage records that the file at cached, as info describes it,
// matched checksum. Best-effort: an image without a stamp is hashed again the
// next time it is used.
func stampCachedImage(cached, checksum string, info os.FileInfo) {
	stamp := fmt.Sprintf("%s %d %d\n", strings.ToLower(checksum), info.Size(), info.ModTime().UnixNano())
	_ = os.WriteFile(imageStampPath(cached), []byte(stamp), 0o644)
}

// removeCachedImage deletes a cached image, its stamp first, so that a stamp
// never outlives the file it describes.
func removeCachedImage(cached string) {
	_ = os.Remove(imageStampPath(cached))
	_ = os.Remove(cached)
}

// cachedImageUsable reports whether the file at path can be used as the image
// whose manifest checksum is checksum.
//
// With no checksum in the manifest any non-empty file is used, as it was
// before checksums were checked. With one, a file is used when its stamp says
// this very file already matched it. A file with no stamp, or changed since it
// was stamped, is hashed once and stamped if it matches. Anything else is not
// the published image: it is removed, with one line saying why, and the caller
// downloads the image again.
func cachedImageUsable(path, checksum string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return false
	}
	checksum = strings.TrimSpace(checksum)
	if checksum == "" {
		return true
	}
	name := filepath.Base(path)
	if stamp, ok := readImageStamp(path); ok && stamp.size == info.Size() && stamp.mtimeNs == info.ModTime().UnixNano() {
		if strings.EqualFold(stamp.checksum, checksum) {
			return true
		}
		fmt.Printf("Cached image %s is outdated: the published build has changed. Downloading the current build.\n", name)
	} else {
		fmt.Printf("Checking cached image %s against the published checksum...\n", name)
		got, err := sha256File(path)
		switch {
		case err != nil:
			fmt.Printf("Cached image %s could not be checked (%v). Downloading it again.\n", name, err)
		case !strings.EqualFold(got, checksum):
			fmt.Printf("Cached image %s does not match the published checksum. Downloading it again.\n", name)
		default:
			stampCachedImage(path, checksum, info)
			return true
		}
	}
	removeCachedImage(path)
	return false
}

// imageChecksumError reports a finished download that is not the image the
// manifest describes.
type imageChecksumError struct{ got, want string }

func (e *imageChecksumError) Error() string {
	return fmt.Sprintf("the downloaded image does not match the checksum in the manifest (got %s, manifest says %s), so it was not kept; the build may have changed during the download, try again", e.got, e.want)
}

// verifyDownloadedImage checks the finished download at tmp against the
// checksum the manifest publishes for it, if any. A file that fails is removed.
func verifyDownloadedImage(tmp, checksum string) error {
	checksum = strings.TrimSpace(checksum)
	if checksum == "" {
		return nil
	}
	fmt.Println("Verifying the download...")
	got, err := sha256File(tmp)
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("verifying the download: %w", err)
	}
	if !strings.EqualFold(got, checksum) {
		os.Remove(tmp)
		return &imageChecksumError{got: got, want: checksum}
	}
	return nil
}

// moveToImageCache moves the verified download at tmp to cached, and stamps
// it when the manifest publishes a checksum. The stamp of the file it replaces
// goes first and the new stamp is written last, once the whole file is in
// place, so a stamp never sits beside a file it was not computed from.
func moveToImageCache(tmp, cached, checksum string) error {
	// Also clears a stale or 0-byte file so Rename succeeds on Windows.
	removeCachedImage(cached)
	if err := os.Rename(tmp, cached); err != nil {
		os.Remove(tmp)
		return err
	}
	if checksum = strings.TrimSpace(checksum); checksum != "" {
		if info, err := os.Stat(cached); err == nil {
			stampCachedImage(cached, checksum, info)
		}
	}
	return nil
}

// recheckCachedImage hashes the cached image at path again, whatever its stamp
// says, before the full-image fallback writes all of it. A block-map write
// has just failed, most often because part of this file is not what the block
// map describes: then either the block map or the file is wrong, and writing
// the file in full is only safe when it is still the published image. A file
// that is not is removed, so the next install downloads it again. With no
// checksum in the manifest there is nothing to check against, and the
// fallback goes ahead as before.
func recheckCachedImage(path, checksum string) error {
	checksum = strings.TrimSpace(checksum)
	if checksum == "" {
		return nil
	}
	fmt.Println("Fast write failed. Checking the cached image before writing all of it...")
	got, err := sha256File(path)
	if err != nil {
		return fmt.Errorf("checking the cached image: %w", err)
	}
	if !strings.EqualFold(got, checksum) {
		removeCachedImage(path)
		return fmt.Errorf("the cached image %s does not match the published checksum; it was removed from the cache, so run the install again to download it", filepath.Base(path))
	}
	return nil
}
