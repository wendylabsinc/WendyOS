package modelhub

import "testing"

func TestAutomaticAcceleratorIncludesThorAndUnknownVRAM(t *testing.T) {
	for _, tc := range []struct {
		name     string
		hardware Hardware
		want     uint32
	}{
		{"Thor JetPack 7", Hardware{CUDA: true, JetPack: "7.2", SharedMemory: true}, 1},
		{"JetPack 6", Hardware{CUDA: true, JetPack: "6.2"}, 3},
		{"JetPack 5", Hardware{CUDA: true, JetPack: "5.1"}, 2},
		{"CUDA unknown memory", Hardware{CUDA: true}, 1},
		{"CPU only", Hardware{}, 0},
		{"unsupported GPU", Hardware{GPU: "other"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.hardware.Accelerator(); got != tc.want {
				t.Fatalf("accelerator = %d, want %d", got, tc.want)
			}
		})
	}
}
