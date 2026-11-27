package hardware

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeGPIOFixture(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for path, text := range files {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func gpioSummary(t *testing.T, root string, chipInfo func(string) (string, uint32, bool)) [][2]string {
	t.Helper()
	var got [][2]string
	for _, c := range discoverGPIOChips(root, chipInfo) {
		if c.GetCategory() != "gpio" {
			t.Errorf("category %q", c.GetCategory())
		}
		got = append(got, [2]string{c.GetDevicePath(), c.GetDescription()})
	}
	return got
}

// A kernel with CONFIG_GPIO_SYSFS keeps the sysfs entries it always reported.
func TestDiscoverGPIOKeepsSysfsChips(t *testing.T) {
	root := t.TempDir()
	writeGPIOFixture(t, root, map[string]string{
		"sys/class/gpio/export":            "",
		"sys/class/gpio/gpiochip512/label": "pinctrl-rp1\n",
		"sys/class/gpio/gpiochip512/ngpio": "54\n",
		"sys/class/gpio/gpiochip566/label": "",
		"sys/class/gpio/gpio17/value":      "0",
		"dev/gpiochip0":                    "",
		"dev/gpiochip1":                    "",
	})
	got := gpioSummary(t, root, func(string) (string, uint32, bool) {
		t.Fatal("a sysfs host must not open the character devices")
		return "", 0, false
	})
	want := [][2]string{
		{"/sys/class/gpio/gpiochip512", "gpiochip512 (pinctrl-rp1, 54 lines)"},
		{"/sys/class/gpio/gpiochip566", "gpiochip566"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// Without CONFIG_GPIO_SYSFS (the Orange Pi 5 Pro's kernel) the chips come
// from /dev, in chip order, labelled from the chip itself.
func TestDiscoverGPIOFallsBackToCharacterDevices(t *testing.T) {
	root := t.TempDir()
	writeGPIOFixture(t, root, map[string]string{
		"dev/gpiochip10":  "",
		"dev/gpiochip0":   "",
		"dev/gpiochip1":   "",
		"dev/gpiochip2":   "",
		"dev/gpiochipfoo": "",
		"dev/gpiochip01":  "",
	})
	chipInfo := func(dev string) (string, uint32, bool) {
		switch dev {
		case filepath.Join(root, "dev/gpiochip0"):
			return "gpio0", 32, true
		case filepath.Join(root, "dev/gpiochip1"):
			return "", 32, true
		}
		return "", 0, false
	}
	got := gpioSummary(t, root, chipInfo)
	want := [][2]string{
		{"/dev/gpiochip0", "gpiochip0 (gpio0, 32 lines)"},
		{"/dev/gpiochip1", "gpiochip1"},
		{"/dev/gpiochip2", "gpiochip2"},
		{"/dev/gpiochip10", "gpiochip10"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestDiscoverGPIONoChips(t *testing.T) {
	root := t.TempDir()
	writeGPIOFixture(t, root, map[string]string{"sys/class/gpio/export": ""})
	if got := discoverGPIOChips(root, gpioChipInfo); len(got) != 0 {
		t.Errorf("got %v, want no chips", got)
	}
}

// A node that is not a GPIO chip leaves the description unlabelled.
func TestGPIOChipInfoRejectsNonChips(t *testing.T) {
	regular := filepath.Join(t.TempDir(), "gpiochip0")
	writeGPIOFixture(t, filepath.Dir(regular), map[string]string{"gpiochip0": ""})
	for _, dev := range []string{regular, filepath.Join(t.TempDir(), "absent")} {
		if label, lines, ok := gpioChipInfo(dev); ok {
			t.Errorf("gpioChipInfo(%s) = %q, %d, true", dev, label, lines)
		}
	}
}
