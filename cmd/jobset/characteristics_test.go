package jobset

import (
	"testing"
)

func TestGetSystemCharacteristics(t *testing.T) {
	tests := []struct {
		deviceType      string
		wantTopology    string
		wantAccelerator string
		wantErr         bool
	}{
		{
			deviceType:      "gpu-l4-1",
			wantTopology:    "N/A",
			wantAccelerator: "nvidia-l4",
			wantErr:         false,
		},
		{
			deviceType: "tpu-7x-16",
			// Let's check the map generation logic.
			// tpu7x prefix. 2 tensorcores per chip.
			// 2x2x1 topology -> 4 chips -> 8 tensorcores. Device type: tpu7x-8.
			// 2x2x2 topology -> 8 chips -> 16 tensorcores. Device type: tpu7x-16.
			wantTopology:    "2x2x2",
			wantAccelerator: "tpu7x",
			wantErr:         false,
		},
		{
			deviceType:      "tpu-7x-2048",
			wantTopology:    "8x8x16",
			wantAccelerator: "tpu7x",
			wantErr:         false,
		},
		{
			deviceType:      "tpu-7x-2x2x4",
			wantTopology:    "2x2x4",
			wantAccelerator: "tpu7x",
			wantErr:         false,
		},
		{
			deviceType:      "tpu-v6e-256",
			wantTopology:    "16x16",
			wantAccelerator: "tpu-v6e-slice",
			wantErr:         false,
		},
		{
			deviceType:      "tpu-v6e-16x16",
			wantTopology:    "16x16",
			wantAccelerator: "tpu-v6e-slice",
			wantErr:         false,
		},
		{
			deviceType: "unknown-device",
			wantErr:    true,
		},
		{
			// Only gpu-l4-8 would be a valid device type.
			deviceType: "gpu-l4-9",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.deviceType, func(t *testing.T) {
			got, err := GetSystemCharacteristics(tt.deviceType)
			if (err != nil) != tt.wantErr {
				t.Errorf("GetSystemCharacteristics() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !tt.wantErr {
				if got.Topology != tt.wantTopology {
					t.Errorf("GetSystemCharacteristics() Topology = %v, want %v", got.Topology, tt.wantTopology)
				}
				if got.GKEAccelerator != tt.wantAccelerator {
					t.Errorf("GetSystemCharacteristics() GKEAccelerator = %v, want %v", got.GKEAccelerator, tt.wantAccelerator)
				}
			}
		})
	}
}

func TestComputeChipsPerVM(t *testing.T) {
	tests := []struct {
		topology string
		want     int
	}{
		{"1x1x1", 1},
		{"2x2x1", 4},
		{"2x2x2", 4},
		{"2x2x4", 4},
	}

	for _, tt := range tests {
		t.Run(tt.topology, func(t *testing.T) {
			if got := computeChipsPerVM(tt.topology); got != tt.want {
				t.Errorf("computeChipsPerVM() = %v, want %v", got, tt.want)
			}
		})
	}
}
