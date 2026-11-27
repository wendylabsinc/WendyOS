//go:build darwin || linux || windows

package commands

import (
	"archive/zip"
	"bytes"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Cached OS images: `wendy os download` writes the cache entry that
// `wendy install` reads, and a cached or downloaded image is used only when it
// matches the checksum its manifest publishes.

// publishChecksums gives img the checksums a manifest publishes for the image
// s serves now.
func publishChecksums(s *flashSourceServer, img *imageInfo) {
	sum := blockChecksum(s.zst)
	img.Checksum, img.ZstChecksum = sum, sum
}

// rebuiltSparseImage is buildSparseImage as a later build of the same version:
// the same layout holding other data.
func rebuiltSparseImage() ([]byte, *Bmap) {
	const bs = 4096
	img, b := buildSparseImage()
	for i := bs * 4; i < bs*6; i++ {
		img[i] = 0x02
	}
	b.Ranges[1].Checksum = blockChecksum(img[bs*4 : bs*6])
	return img, b
}

// resolveFlash is resolveFlashSource for img on the flash test board, with
// what it printed. The caller closes the source with closeFlash.
func resolveFlash(t *testing.T, img *imageInfo, noBmap bool) (*flashSource, string, error) {
	t.Helper()
	var src *flashSource
	out, err := captureCommandStdout(t, func() (err error) {
		src, err = resolveFlashSource(flashTestDevice, img.Version, img.Storage, img, noBmap)
		return err
	})
	return src, out, err
}

func closeFlash(src *flashSource) {
	if src != nil && src.stream != nil {
		src.stream.Close()
	}
}

// imageStampOf is the checksum stamp kept beside a cached image.
func imageStampOf(cached string) string { return cached + ".sha256" }

// assertImageStamp checks that the stamp beside cached names checksum, or,
// for an empty checksum, that there is no stamp.
func assertImageStamp(t *testing.T, cached, checksum string) {
	t.Helper()
	stamp, err := os.ReadFile(imageStampOf(cached))
	switch {
	case checksum == "" && err == nil:
		t.Errorf("%s has a stamp (%q), want none", filepath.Base(cached), stamp)
	case checksum != "" && !strings.Contains(string(stamp), checksum):
		t.Errorf("stamp of %s = %q (%v), want one naming %s", filepath.Base(cached), stamp, err, checksum)
	}
}

// osCacheFiles lists the OS cache: its images, and everything else in it.
func osCacheFiles(t *testing.T) (images, others []string) {
	t.Helper()
	dir, err := osCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasSuffix(n, ".bmap") || strings.HasSuffix(n, ".sha256") {
			others = append(others, n)
		} else {
			images = append(images, n)
		}
	}
	return images, others
}

// stubImageDownload makes every image download produce what payload returns,
// whatever URL it names, and counts the downloads.
func stubImageDownload(t *testing.T, payload func() []byte) *int {
	t.Helper()
	downloads := 0
	old := downloadOSImage
	t.Cleanup(func() { downloadOSImage = old })
	downloadOSImage = func(*imageInfo) (string, error) {
		downloads++
		dir, err := osCacheDir()
		if err != nil {
			return "", err
		}
		f, err := os.CreateTemp(dir, "wendyos-*.img")
		if err != nil {
			return "", err
		}
		defer f.Close()
		_, err = f.Write(payload())
		return f.Name(), err
	}
	return &downloads
}

// zipOf wraps image in a release zip, as older manifests publish it.
func zipOf(t *testing.T, image []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	fw, err := w.Create("wendyos.img")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(image); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const (
	downloadTestDevice  = "raspberry-pi-5"
	downloadTestVersion = "0.21.0"
)

// downloadTestDeviceInfo lists one version of a disk-image board. The seekable
// form is how the Raspberry Pi 5 0.21.0 entry looks: one .wic.zst as both path
// and zst_path, beside its block map. The other form is the older release zip.
func downloadTestDeviceInfo(seekable bool, checksum string) deviceInfo {
	v := deviceVersion{Path: "images/test/wendyos.wic.zip", Checksum: checksum}
	if seekable {
		v = deviceVersion{
			Path: "images/test/wendyos.wic.zst", Checksum: checksum,
			ZstPath: "images/test/wendyos.wic.zst", ZstChecksum: checksum,
			BmapPath: "images/test/wendyos.wic.bmap",
		}
	}
	return deviceInfo{
		Name:     "Raspberry Pi 5",
		Manifest: &deviceManifest{Versions: map[string]deviceVersion{downloadTestVersion: v}},
	}
}

// installFlashSource resolves what `wendy install` would flash from dev to an
// SD card in a USB reader. The block map comes from s; the image must not.
func installFlashSource(t *testing.T, s *flashSourceServer, dev deviceInfo, noBmap bool) (*flashSource, string) {
	t.Helper()
	storage := manifestStorage(dev.Manifest.Versions[downloadTestVersion], StorageUSB, false, "")
	img, err := getImageInfo(dev.Manifest, downloadTestVersion, storage)
	if err != nil {
		t.Fatal(err)
	}
	if img.BmapURL != "" {
		img.BmapURL = s.URL + "/image.img.bmap"
	}
	src, err := resolveFlashSource(downloadTestDevice, downloadTestVersion, storage, img, noBmap)
	if err != nil {
		t.Fatalf("resolveFlashSource: %v", err)
	}
	zstPath, err := osCachedZstPath(downloadTestDevice, downloadTestVersion, storage)
	if err != nil {
		t.Fatal(err)
	}
	return src, zstPath
}

// Item (a): `wendy os download` followed by `wendy install` for the same
// device, version and storage must flash the downloaded file, not fetch the
// same bytes again under another cache name.
func TestOSDownloadThenInstallUsesTheCachedImage(t *testing.T) {
	for _, tt := range []struct {
		name        string
		zip         bool // the manifest publishes a release zip and no seekable image
		noBmap      bool // install --no-bmap
		noChecksums bool // the manifest publishes no checksum
	}{
		{name: "seekable image"},
		{name: "seekable image, --no-bmap", noBmap: true},
		{name: "seekable image, no checksums", noChecksums: true},
		{name: "release zip", zip: true},
		{name: "release zip, no checksums", zip: true, noChecksums: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newFlashSourceServer(t, bmapServed)
			published := s.zst
			if tt.zip {
				published = zipOf(t, s.raw)
			}
			// getImageInfo names the public bucket; serve those URLs here.
			downloads := stubImageDownload(t, func() []byte { return published })
			prompts := stubConfirmRedownload(t, false, nil)
			checksum := blockChecksum(published)
			if tt.noChecksums {
				checksum = ""
			}
			dev := downloadTestDeviceInfo(!tt.zip, checksum)

			if err := downloadOSVersion(downloadTestDevice, dev, downloadTestVersion, "", false); err != nil {
				t.Fatalf("os download: %v", err)
			}
			if *downloads != 1 || *prompts != 0 {
				t.Fatalf("os download: %d downloads, %d prompts; want one download and no prompt", *downloads, *prompts)
			}

			src, zstPath := installFlashSource(t, s, dev, tt.noBmap)
			defer closeFlash(src)
			if *downloads != 1 {
				t.Errorf("install downloaded the image again: %d downloads in all, want 1", *downloads)
			}
			if images, _ := osCacheFiles(t); len(images) != 1 {
				t.Errorf("cache holds %v, want one copy of the image", images)
			}
			assertFlashSource(t, s, src, zstPath, !tt.zip && !tt.noBmap)
		})
	}
}

// `wendy os download` run again: a current cached image is offered for
// replacement as before, and one the manifest no longer describes is replaced
// without asking.
func TestOSDownloadWithTheImageAlreadyCached(t *testing.T) {
	for _, tt := range []struct {
		name          string
		overwrite     bool
		republished   bool
		wantDownloads int
		wantPrompts   int
	}{
		{name: "current image, declined", wantDownloads: 1, wantPrompts: 1},
		{name: "current image, --overwrite", overwrite: true, wantDownloads: 2},
		{name: "republished build", republished: true, wantDownloads: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newFlashSourceServer(t, bmapServed)
			downloads := stubImageDownload(t, func() []byte { zst, _ := s.published(); return zst })
			prompts := stubConfirmRedownload(t, false, nil)
			dev := downloadTestDeviceInfo(true, blockChecksum(s.zst))
			if err := downloadOSVersion(downloadTestDevice, dev, downloadTestVersion, "", false); err != nil {
				t.Fatalf("os download: %v", err)
			}

			if tt.republished {
				raw, b := rebuiltSparseImage()
				s.publish(t, raw, b)
				dev = downloadTestDeviceInfo(true, blockChecksum(s.zst))
			}
			if err := downloadOSVersion(downloadTestDevice, dev, downloadTestVersion, "", tt.overwrite); err != nil {
				t.Fatalf("second os download: %v", err)
			}
			if *downloads != tt.wantDownloads || *prompts != tt.wantPrompts {
				t.Errorf("%d downloads and %d prompts, want %d and %d", *downloads, *prompts, tt.wantDownloads, tt.wantPrompts)
			}
			zstPath, err := osCachedZstPath(downloadTestDevice, downloadTestVersion, "sd")
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(zstPath); err != nil || !bytes.Equal(got, s.zst) {
				t.Errorf("%s does not hold the published image (%v)", zstPath, err)
			}
			assertImageStamp(t, zstPath, blockChecksum(s.zst))
		})
	}
}

// Item (b): a cached file that is not the published image must not be flashed.
func TestCorruptCachedImageIsNotUsed(t *testing.T) {
	for _, tt := range []struct {
		name    string
		noBmap  bool
		checked bool // the file passed its check on an earlier run and was cut short since
	}{
		{name: "never checked"},
		{name: "never checked, --no-bmap", noBmap: true},
		{name: "cut short after it was checked", checked: true},
		{name: "cut short after it was checked, --no-bmap", noBmap: true, checked: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, img := newFlashSourceServer(t, bmapServed)
			publishChecksums(s, img)
			zstPath := flashCachePath(t, ".img.zst")
			if tt.checked {
				writeFlashCacheFile(t, ".img.zst", s.zst)
				src, _, err := resolveFlash(t, img, tt.noBmap)
				closeFlash(src)
				if err != nil || s.downloads.Load() != 0 {
					t.Fatalf("first use of the intact image: err=%v, %d downloads", err, s.downloads.Load())
				}
				if err := os.Truncate(zstPath, int64(len(s.zst)/2)); err != nil {
					t.Fatal(err)
				}
			} else {
				writeFlashCacheFile(t, ".img.zst", bytes.Repeat([]byte{0xee}, len(s.zst)))
			}

			src, out, err := resolveFlash(t, img, tt.noBmap)
			defer closeFlash(src)
			if err != nil {
				t.Fatalf("resolveFlashSource: %v", err)
			}
			if n := s.downloads.Load(); n != 1 {
				t.Errorf("downloaded the image %d times, want once", n)
			}
			if n := strings.Count(out, "does not match the published checksum"); n != 1 {
				t.Errorf("%d lines say why the cached image was dropped, want 1; output:\n%s", n, out)
			}
			if got, err := os.ReadFile(zstPath); err == nil && !bytes.Equal(got, s.zst) {
				t.Errorf("the corrupt image is still cached at %s", zstPath)
			}
			assertFlashSource(t, s, src, zstPath, !tt.noBmap)
		})
	}
}

// The zip and raw-image cache entries get the same check as the seekable one.
func TestResolveOSImageChecksTheCachedFile(t *testing.T) {
	for _, tt := range []struct{ name, url, ext string }{
		{"release zip", "https://example.com/wendyos.wic.zip", ".zip"},
		{"raw image", "https://example.com/wendyos.sdimg.gz", ".img"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			isolateVMImageCache(t)
			published := []byte("the published image")
			downloads := stubImageDownload(t, func() []byte { return published })
			img := &imageInfo{Version: flashTestVersion, DownloadURL: tt.url, Checksum: blockChecksum(published)}
			cached, err := osCachedPath(flashTestDevice, flashTestVersion, "", tt.ext)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(cached, []byte("something else entirely"), 0o644); err != nil {
				t.Fatal(err)
			}

			for run := 1; run <= 2; run++ {
				got, err := resolveOSImage(flashTestDevice, img)
				if err != nil || got != cached {
					t.Fatalf("run %d: resolveOSImage = %q, %v; want %q", run, got, err, cached)
				}
				if data, _ := os.ReadFile(cached); !bytes.Equal(data, published) {
					t.Fatalf("run %d: %s does not hold the published image", run, cached)
				}
				if *downloads != 1 {
					t.Fatalf("run %d: %d downloads in all, want 1", run, *downloads)
				}
			}
			assertImageStamp(t, cached, img.Checksum)
		})
	}
}

// Item (c): a push to a PR republishes "pr-N" with a new image, block map and
// checksum. The build cached under that unchanged name must not be flashed.
func TestRepushedPRImageIsDownloadedAgain(t *testing.T) {
	for _, tt := range []struct {
		name   string
		noBmap bool
		ext    string // the cache entry this path fills
	}{
		{name: "block map write", ext: ".img.zst"},
		{name: "--no-bmap", noBmap: true, ext: ".img"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, img := newFlashSourceServer(t, bmapServed)
			img.Version = "pr-12"
			publishChecksums(s, img)
			src, _, err := resolveFlash(t, img, tt.noBmap)
			closeFlash(src)
			if err != nil || s.downloads.Load() != 1 {
				t.Fatalf("first build: err=%v, %d downloads", err, s.downloads.Load())
			}
			firstBuild := s.zst

			raw, b := rebuiltSparseImage()
			s.publish(t, raw, b)
			publishChecksums(s, img)
			src, out, err := resolveFlash(t, img, tt.noBmap)
			if err != nil {
				t.Fatalf("resolveFlashSource after the push: %v", err)
			}
			if n := s.downloads.Load(); n != 2 {
				t.Errorf("%d downloads over two builds, want 2: the first build was flashed again", n)
			}
			if n := strings.Count(out, "is outdated"); n != 1 {
				t.Errorf("%d lines say why the cached image was dropped, want 1; output:\n%s", n, out)
			}
			cached, err := osCachedPath(flashTestDevice, img.Version, flashTestStorage, tt.ext)
			if err != nil {
				t.Fatal(err)
			}
			if got, err := os.ReadFile(cached); err != nil || !bytes.Equal(got, s.zst) {
				t.Errorf("%s does not hold the new build (%v)", cached, err)
			}
			if bytes.Equal(firstBuild, s.zst) {
				t.Fatal("test images do not differ")
			}
			assertImageStamp(t, cached, img.ZstChecksum)
			assertFlashSource(t, s, src, cached, !tt.noBmap)
			closeFlash(src)

			// The new build is then reused like any other cached image.
			src, out, err = resolveFlash(t, img, tt.noBmap)
			closeFlash(src)
			if err != nil || s.downloads.Load() != 2 || strings.Contains(out, "is outdated") {
				t.Errorf("third run: err=%v, %d downloads, output:\n%s", err, s.downloads.Load(), out)
			}
		})
	}
}

// A cache file an older wendy left has no stamp. It is hashed once, stamped,
// and from then on trusted without being read again. A stamp that cannot be
// read as one counts as none.
func TestUnstampedCachedImageIsCheckedOnce(t *testing.T) {
	for _, tt := range []struct{ name, stamp string }{
		{name: "no stamp"},
		{name: "unreadable stamp", stamp: "not a stamp\n"},
		{name: "checksum alone, as a flashpack stamp has", stamp: "<checksum>\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s, img := newFlashSourceServer(t, bmapServed)
			publishChecksums(s, img)
			zstPath := writeFlashCacheFile(t, ".img.zst", s.zst)
			if tt.stamp != "" {
				stamp := strings.ReplaceAll(tt.stamp, "<checksum>", img.ZstChecksum)
				if err := os.WriteFile(imageStampOf(zstPath), []byte(stamp), 0o644); err != nil {
					t.Fatal(err)
				}
			}

			src, out, err := resolveFlash(t, img, false)
			if err != nil {
				t.Fatalf("resolveFlashSource: %v", err)
			}
			if n := s.downloads.Load(); n != 0 {
				t.Errorf("downloaded an intact cached image %d times", n)
			}
			if !strings.Contains(out, "Checking cached image") {
				t.Errorf("the one-time check was not announced; output:\n%s", out)
			}
			assertFlashSource(t, s, src, zstPath, true)
			stamp, err := os.ReadFile(imageStampOf(zstPath))
			if err != nil || !strings.HasPrefix(string(stamp), img.ZstChecksum+" ") {
				t.Fatalf("stamp = %q (%v), want one naming %s and the file", stamp, err, img.ZstChecksum)
			}

			// Hashing a multi-gigabyte image on every install is what the stamp
			// is for: a stamped file whose size and modification time are
			// unchanged is not read again. Same-size bytes with the old time
			// show that.
			info, err := os.Stat(zstPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(zstPath, bytes.Repeat([]byte{0xee}, len(s.zst)), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(zstPath, info.ModTime(), info.ModTime()); err != nil {
				t.Fatal(err)
			}
			src, out, err = resolveFlash(t, img, false)
			if err != nil || src.seekableZst != zstPath || s.downloads.Load() != 0 || strings.Contains(out, "Checking cached image") {
				t.Errorf("stamped image was not taken on trust: err=%v, %d downloads, output:\n%s", err, s.downloads.Load(), out)
			}
		})
	}
}

// A stamp speaks only for the file it was computed from. A file put in its
// place since, as an older wendy's `os download --overwrite` does without
// touching the stamp, is judged by its own contents.
func TestCachedImageReplacedBehindItsStampIsHashedAgain(t *testing.T) {
	s, img := newFlashSourceServer(t, bmapServed)
	img.Version = "pr-12"
	publishChecksums(s, img)
	src, _, err := resolveFlash(t, img, false)
	closeFlash(src)
	if err != nil || s.downloads.Load() != 1 {
		t.Fatalf("first build: err=%v, %d downloads", err, s.downloads.Load())
	}
	zstPath, err := osCachedPath(flashTestDevice, img.Version, flashTestStorage, ".img.zst")
	if err != nil {
		t.Fatal(err)
	}
	assertImageStamp(t, zstPath, img.ZstChecksum)

	// The next build is published and lands in the cache with the first
	// build's stamp still beside it.
	raw, b := rebuiltSparseImage()
	s.publish(t, raw, b)
	publishChecksums(s, img)
	if err := os.WriteFile(zstPath, s.zst, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Second)
	if err := os.Chtimes(zstPath, later, later); err != nil {
		t.Fatal(err)
	}

	src, out, err := resolveFlash(t, img, false)
	defer closeFlash(src)
	if err != nil {
		t.Fatalf("resolveFlashSource: %v", err)
	}
	if n := s.downloads.Load(); n != 1 {
		t.Errorf("%d downloads, want 1: the cached file is the published build", n)
	}
	if !strings.Contains(out, "Checking cached image") || strings.Contains(out, "is outdated") {
		t.Errorf("the replaced file was not hashed; output:\n%s", out)
	}
	assertImageStamp(t, zstPath, img.ZstChecksum)
	assertFlashSource(t, s, src, zstPath, true)
}

// With no checksum in the manifest there is nothing to check against, and the
// cache behaves as it always has.
func TestCachedImageWithoutManifestChecksumIsUsedAsBefore(t *testing.T) {
	t.Run("a cached file is used as it is", func(t *testing.T) {
		s, img := newFlashSourceServer(t, bmapServed)
		zstPath := writeFlashCacheFile(t, ".img.zst", []byte("whatever an earlier run left"))
		src, out, err := resolveFlash(t, img, false)
		if err != nil || src.seekableZst != zstPath || s.downloads.Load() != 0 {
			t.Fatalf("err=%v, source=%+v, %d downloads; want the cached file", err, src, s.downloads.Load())
		}
		if strings.Contains(out, "Checking cached image") {
			t.Errorf("checked an image the manifest has no checksum for; output:\n%s", out)
		}
		assertImageStamp(t, zstPath, "")
	})

	t.Run("a republished build is not noticed", func(t *testing.T) {
		s, img := newFlashSourceServer(t, bmapServed)
		img.Version = "pr-12"
		zstPath, err := osCachedPath(flashTestDevice, img.Version, flashTestStorage, ".img.zst")
		if err != nil {
			t.Fatal(err)
		}
		src, _, err := resolveFlash(t, img, false)
		if err != nil || s.downloads.Load() != 1 {
			t.Fatalf("first build: err=%v, %d downloads", err, s.downloads.Load())
		}
		assertFlashSource(t, s, src, zstPath, true)
		assertImageStamp(t, zstPath, "")
		firstBuild := s.zst

		raw, b := rebuiltSparseImage()
		s.publish(t, raw, b)
		if _, _, err := resolveFlash(t, img, false); err != nil || s.downloads.Load() != 1 {
			t.Errorf("after the push: err=%v, %d downloads; want the cached build as before", err, s.downloads.Load())
		}
		if got, _ := os.ReadFile(zstPath); !bytes.Equal(got, firstBuild) {
			t.Error("the cached build was replaced")
		}
	})
}

// A download that is not the image the manifest describes is an error, and
// nothing of it is kept.
func TestDownloadedImageFailingItsChecksumIsAnError(t *testing.T) {
	wrong := blockChecksum([]byte("the build the manifest describes"))
	assertRejected := func(t *testing.T, err error, downloads int) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "does not match") || !strings.Contains(err.Error(), wrong) {
			t.Errorf("err = %v, want a checksum mismatch naming the manifest checksum", err)
		}
		if downloads != 1 {
			t.Errorf("%d downloads, want one: a second attempt cannot match either", downloads)
		}
		if images, others := osCacheFiles(t); len(images)+len(others) != 0 {
			t.Errorf("a rejected download left %v %v in the cache", images, others)
		}
	}

	t.Run("seekable image", func(t *testing.T) {
		s, img := newFlashSourceServer(t, bmapServed)
		img.Checksum, img.ZstChecksum = wrong, wrong
		src, _, err := resolveFlash(t, img, false)
		closeFlash(src)
		assertRejected(t, err, int(s.downloads.Load()))
	})

	t.Run("release zip", func(t *testing.T) {
		isolateVMImageCache(t)
		downloads := stubImageDownload(t, func() []byte { return []byte("some other build") })
		_, err := resolveOSImage(flashTestDevice, &imageInfo{
			Version: flashTestVersion, DownloadURL: "https://example.com/wendyos.wic.zip", Checksum: wrong,
		})
		assertRejected(t, err, *downloads)
	})
}

// The stamp is written only for a complete, verified file: a download that
// fails leaves neither file nor stamp, and one that drops and resumes is
// stamped once it is whole.
func TestImageDownloadInterruptedOrResumed(t *testing.T) {
	payload := testImagePayload(8 << 10)
	sum := blockChecksum(payload)
	serve := func(t *testing.T, s *flakyImageServer) *imageInfo {
		t.Helper()
		s.payload = payload
		srv := httptest.NewServer(s)
		t.Cleanup(srv.Close)
		url := srv.URL + "/image.img.zst"
		return &imageInfo{
			DownloadURL: url, ZstURL: url, BmapURL: srv.URL + "/image.img.bmap",
			Checksum: sum, ZstChecksum: sum,
			Version: flashTestVersion, Storage: flashTestStorage,
		}
	}
	// downloadImage draws a progress TUI, which needs a terminal.
	old := downloadOSImage
	t.Cleanup(func() { downloadOSImage = old })
	downloadOSImage = func(img *imageInfo) (string, error) { return downloadImageInto(img, nil) }

	t.Run("interrupted", func(t *testing.T) {
		useTestDownloadPolicy(t, 200*time.Millisecond)
		isolateVMImageCache(t)
		img := serve(t, &flakyImageServer{ranges: true, failAt: 3000, failEvery: true})
		src, _, err := resolveFlash(t, img, false)
		closeFlash(src)
		if err == nil {
			t.Fatal("a download that never completes resolved an image")
		}
		if images, others := osCacheFiles(t); len(images)+len(others) != 0 {
			t.Errorf("an interrupted download left %v %v in the cache", images, others)
		}
	})

	t.Run("interrupted while replacing an outdated image", func(t *testing.T) {
		useTestDownloadPolicy(t, 200*time.Millisecond)
		isolateVMImageCache(t)
		zstPath := flashCachePath(t, ".img.zst")
		// An earlier build, cached and checked on an earlier run.
		earlier := serve(t, &flakyImageServer{ranges: true})
		src, _, err := resolveFlash(t, earlier, false)
		closeFlash(src)
		if err != nil {
			t.Fatalf("caching the earlier build: %v", err)
		}
		assertImageStamp(t, zstPath, sum)

		img := serve(t, &flakyImageServer{ranges: true, failAt: 3000, failEvery: true})
		newSum := blockChecksum([]byte("the build published since"))
		img.Checksum, img.ZstChecksum = newSum, newSum
		src, _, err = resolveFlash(t, img, false)
		closeFlash(src)
		if err == nil {
			t.Fatal("a download that never completes resolved an image")
		}
		if stamp, err := os.ReadFile(imageStampOf(zstPath)); err == nil && strings.Contains(string(stamp), newSum) {
			t.Errorf("the new build is stamped as cached, but its download failed")
		}
		if images, _ := osCacheFiles(t); len(images) != 0 {
			t.Errorf("the outdated image is still cached: %v", images)
		}
	})

	t.Run("resumed", func(t *testing.T) {
		useTestDownloadPolicy(t, 200*time.Millisecond)
		isolateVMImageCache(t)
		s := &flakyImageServer{ranges: true, failAt: 3000}
		img := serve(t, s)
		src, _, err := resolveFlash(t, img, false)
		closeFlash(src)
		if err != nil {
			t.Fatalf("download did not survive the drop: %v", err)
		}
		if !s.failed.Load() {
			t.Fatal("the connection was never dropped")
		}
		zstPath := flashCachePath(t, ".img.zst")
		if got, err := os.ReadFile(zstPath); err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("%s does not hold the whole image (%v)", zstPath, err)
		}
		assertImageStamp(t, zstPath, sum)
	})
}

// Image checksum stamps are bookkeeping like the flashpack ones, not downloads.
func TestListOSCacheEntriesLeavesOutImageStamps(t *testing.T) {
	dir := t.TempDir()
	for name, size := range map[string]int{
		"raspberry-pi-5-0.21.0-sd.img.zst":        10,
		"raspberry-pi-5-0.21.0-sd.img.zst.sha256": 90,
		"raspberry-pi-5-0.21.0-sd.bmap":           5,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	items, err := listOSCacheEntries(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, it := range items {
		names = append(names, it.Name)
	}
	if len(names) != 2 || names[0] != "raspberry-pi-5-0.21.0-sd.bmap" || names[1] != "raspberry-pi-5-0.21.0-sd.img.zst" {
		t.Errorf("entries = %v, want the image and its block map only", names)
	}
}

// After a block-map write fails, the full-image fallback hashes the cached
// file again rather than trust its stamp, and refuses one that is not the
// published image.
func TestRecheckCachedImageBeforeFullWrite(t *testing.T) {
	isolateVMImageCache(t)
	published := []byte("the published image")
	checksum := blockChecksum(published)
	cached := writeFlashCacheFile(t, ".img.zst", published)
	if !cachedImageUsable(cached, checksum) {
		t.Fatal("the published image was not accepted")
	}
	if err := recheckCachedImage(cached, checksum); err != nil {
		t.Fatalf("recheck of the published image: %v", err)
	}
	assertImageStamp(t, cached, checksum)

	// The file goes bad with its size and time unchanged, as on a failing
	// disk, so its stamp still vouches for it.
	info, err := os.Stat(cached)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cached, bytes.Repeat([]byte{0xee}, len(published)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(cached, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if !cachedImageUsable(cached, checksum) {
		t.Fatal("setup: the stamp no longer vouches for the file")
	}
	err = recheckCachedImage(cached, checksum)
	if err == nil || !strings.Contains(err.Error(), "does not match the published checksum") {
		t.Errorf("recheck of a damaged image: err = %v, want a checksum mismatch", err)
	}
	if images, others := osCacheFiles(t); len(images)+len(others) != 0 {
		t.Errorf("the damaged image was left in the cache: %v %v", images, others)
	}

	// Without a manifest checksum the fallback goes ahead as before.
	unchecked := writeFlashCacheFile(t, ".img.zst", []byte("whatever an earlier run left"))
	if err := recheckCachedImage(unchecked, ""); err != nil {
		t.Errorf("recheck without a manifest checksum: %v", err)
	}
	if _, err := os.Stat(unchecked); err != nil {
		t.Errorf("recheck without a manifest checksum removed the image: %v", err)
	}
}
