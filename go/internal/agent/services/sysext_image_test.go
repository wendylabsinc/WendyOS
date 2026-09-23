package services

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// wendyos-hello.raw is the image the driver pipeline actually produced and that
// was validated on an RPi5, so this pins compatibility with real mksquashfs
// output rather than with something a Go library round-tripped for itself.
const (
	fixtureName   = "wendyos-hello"
	fixtureKernel = "6.18.33-v8-16k"
)

func TestReadExtensionRelease(t *testing.T) {
	got, err := readExtensionRelease(filepath.Join("testdata", fixtureName+".raw"), fixtureName)
	if err != nil {
		t.Fatalf("readExtensionRelease: %v", err)
	}
	for k, want := range map[string]string{
		"ID":                       "wendyos",
		"SYSEXT_LEVEL":             "1",
		"ARCHITECTURE":             "arm64",
		"EXTENSION_RELOAD_MANAGER": "1",
		imageKernelField:           fixtureKernel,
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
}

func TestVerifyImageKernel(t *testing.T) {
	hello := filepath.Join("testdata", fixtureName+".raw")
	noKernel := filepath.Join("testdata", "nokernel.raw")

	garbage := filepath.Join(t.TempDir(), "garbage.raw")
	if err := os.WriteFile(garbage, []byte("this is not a squashfs image"), 0o644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		desc     string
		path     string
		name     string
		declared string
		running  string
		wantErr  string
	}{
		{"kernel matches", hello, fixtureName, "", fixtureKernel, ""},
		{"kernel matches and manifest agrees", hello, fixtureName, fixtureKernel, fixtureKernel, ""},
		// A --file install declares no kernel, so only the image itself can refuse
		// an add-on built for another one.
		{"running kernel differs", hello, fixtureName, "", "6.12.87-v8", "was built for kernel"},
		{"manifest disagrees with the image", hello, fixtureName, "9.9.9-wrong", fixtureKernel, "image declares kernel"},
		{"name does not match the image", hello, "other-driver", "", fixtureKernel, "declares no extension-release"},
		// No modules, so no kernel to pin - must stay installable.
		{"image pins no kernel", noKernel, "nokernel", "", fixtureKernel, ""},
		{"not a squashfs", garbage, fixtureName, "", fixtureKernel, "not a readable squashfs"},
		{"missing file", filepath.Join(t.TempDir(), "absent.raw"), fixtureName, "", fixtureKernel, "opening add-on image"},
	}

	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			err := verifyImageKernel(tt.path, tt.name, tt.declared, tt.running)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("verifyImageKernel() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("verifyImageKernel() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestParseKeyValues(t *testing.T) {
	got := parseKeyValues(strings.NewReader(`ID=wendyos
QUOTED="has spaces"
NOT_A_PAIR
EMPTY=
WENDYOS_KERNEL=6.18.33-v8-16k
`))
	for k, want := range map[string]string{
		"ID":             "wendyos",
		"QUOTED":         "has spaces",
		"EMPTY":          "",
		imageKernelField: "6.18.33-v8-16k",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if _, ok := got["NOT_A_PAIR"]; ok {
		t.Error("a line without = became a key")
	}
}

// A self-describing add-on's autoload list lives only inside the image until it
// merges, which is after the point an install has to sample module residency.
func TestImageModules(t *testing.T) {
	got := imageModules(filepath.Join("testdata", fixtureName+".raw"), fixtureName)
	if len(got) != 1 || got[0] != "wendyos_hello" {
		t.Errorf("imageModules = %v, want [wendyos_hello]", got)
	}
	// An add-on that bakes no list, and an unreadable image, are both "no modules"
	// rather than an error: the install still has to proceed.
	if got := imageModules(filepath.Join("testdata", "nokernel.raw"), "nokernel"); got != nil {
		t.Errorf("nokernel modules = %v, want nil", got)
	}
	if got := imageModules(filepath.Join("testdata", "does-not-exist.raw"), "x"); got != nil {
		t.Errorf("missing image = %v, want nil", got)
	}
}

func TestImageModulesPrivatePayload(t *testing.T) {
	got := imageModules(filepath.Join("testdata", "install-private.raw"), "wendyos-hello")
	if len(got) != 1 || got[0] != "wendyos_hello" {
		t.Fatalf("private modules = %v", got)
	}
}
