package commands

import "testing"

func TestGetOTAUpdateURLPicksTheNVMeArtifactOnlyForNVMe(t *testing.T) {
	const ver = "0.20.1"
	withNVMe := &deviceManifest{Versions: map[string]deviceVersion{ver: {
		OTAUpdatePath:     "default.wendy",
		NVMEOTAUpdatePath: "nvme.wendy",
	}}}
	withoutNVMe := &deviceManifest{Versions: map[string]deviceVersion{ver: {
		OTAUpdatePath: "default.wendy",
	}}}
	cases := []struct {
		name   string
		dm     *deviceManifest
		medium string
		want   string
	}{
		{"nvme with an nvme artifact", withNVMe, "nvme", "nvme.wendy"},
		{"nvme without an nvme artifact", withoutNVMe, "nvme", "default.wendy"},
		{"unknown medium", withNVMe, "", "default.wendy"},
		{"sd card", withNVMe, "sd", "default.wendy"},
		{"emmc device whose manifest also lists an nvme artifact", withNVMe, "emmc", "default.wendy"},
		{"usb disk", withNVMe, "usb", "default.wendy"},
		{"ufs", withNVMe, "ufs", "default.wendy"},
		{"image fallback", withNVMe, "disk", "default.wendy"},
		{"match is exact", withNVMe, "NVMe", "default.wendy"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := getOTAUpdateURL(tc.dm, ver, tc.medium)
			if err != nil {
				t.Fatal(err)
			}
			if want := gcsBaseURL + "/" + tc.want; got != want {
				t.Fatalf("getOTAUpdateURL(%q) = %q, want %q", tc.medium, got, want)
			}
		})
	}
}

func TestGetOTAUpdateURLErrors(t *testing.T) {
	dm := &deviceManifest{Versions: map[string]deviceVersion{
		"0.20.1": {NVMEOTAUpdatePath: "nvme.wendy"},
	}}
	if _, err := getOTAUpdateURL(dm, "0.19.0", "nvme"); err == nil {
		t.Fatal("unknown version: want an error")
	}
	if _, err := getOTAUpdateURL(dm, "0.20.1", "sd"); err == nil {
		t.Fatal("no default artifact for a non-nvme medium: want an error")
	}
}
