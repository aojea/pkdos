package jobset

import (
	"fmt"
	"strconv"
	"strings"
)

// AcceleratorType defines the category of the accelerator.
type AcceleratorType string

const (
	AcceleratorTypeTPU AcceleratorType = "TPU"
	AcceleratorTypeGPU AcceleratorType = "GPU"
)

// AcceleratorCharacteristics holds resource and label information for an accelerator type.
type AcceleratorCharacteristics struct {
	ResourceType     string
	AcceleratorLabel string
	MachineLabel     string
}

var acceleratorTypeToCharacteristics = map[AcceleratorType]AcceleratorCharacteristics{
	AcceleratorTypeTPU: {
		ResourceType:     "google.com/tpu",
		AcceleratorLabel: "cloud.google.com/gke-tpu-accelerator",
		MachineLabel:     "cloud.google.com/gke-tpu-topology",
	},
	AcceleratorTypeGPU: {
		ResourceType:     "nvidia.com/gpu",
		AcceleratorLabel: "cloud.google.com/gke-accelerator",
		MachineLabel:     "cloud.google.com/gce-machine-type",
	},
}

// SystemCharacteristics contains the defining characteristics of a specific accelerator system.
type SystemCharacteristics struct {
	Topology               string
	VMsPerSlice            int
	GKEAccelerator         string
	GCEMachineType         string
	ChipsPerVM             int
	AcceleratorType        AcceleratorType
	DeviceType             string
	SupportsSubSlicing     bool
	RequiresWorkloadPolicy bool
}

// userFacingNameToSystemCharacteristics maps user-facing names to their system characteristics.
var userFacingNameToSystemCharacteristics = make(map[string]SystemCharacteristics)

func init() {
	// Initialize the map with all supported configurations.
	// GPU system characteristics
	registerGPUCharacteristics()
	// TPU system characteristics
	registerTPUCharacteristics()
}

func registerGPUCharacteristics() {
	// l4-$CHIPS
	for _, chips := range []int{1, 2, 4, 8} {
		machineType := fmt.Sprintf("g2-standard-%d", chips*12)
		userFacingNameToSystemCharacteristics[fmt.Sprintf("gpu-l4-%d", chips)] = SystemCharacteristics{
			Topology:               "N/A",
			VMsPerSlice:            1,
			GKEAccelerator:         "nvidia-l4",
			GCEMachineType:         machineType,
			ChipsPerVM:             chips,
			AcceleratorType:        AcceleratorTypeGPU,
			DeviceType:             fmt.Sprintf("gpu-l4-%d", chips),
			SupportsSubSlicing:     false,
			RequiresWorkloadPolicy: true,
		}
	}

	// A100-40gb-$CHIPS
	for _, chips := range []int{1, 2, 4, 8} {
		machineType := fmt.Sprintf("a2-highgpu-%dg", chips)
		userFacingNameToSystemCharacteristics[fmt.Sprintf("gpu-a100-40gb-%d", chips)] = SystemCharacteristics{
			Topology:               "N/A",
			VMsPerSlice:            1,
			GKEAccelerator:         "nvidia-tesla-a100",
			GCEMachineType:         machineType,
			ChipsPerVM:             chips,
			AcceleratorType:        AcceleratorTypeGPU,
			DeviceType:             fmt.Sprintf("gpu-a100-40gb-%d", chips),
			SupportsSubSlicing:     false,
			RequiresWorkloadPolicy: true,
		}
	}

	// gb200-4
	userFacingNameToSystemCharacteristics["gpu-gb200-4"] = SystemCharacteristics{
		Topology:               "1x72",
		VMsPerSlice:            1,
		GKEAccelerator:         "nvidia-gb200",
		GCEMachineType:         "a4x-highgpu-4g",
		ChipsPerVM:             4,
		AcceleratorType:        AcceleratorTypeGPU,
		DeviceType:             "gpu-gb200-4",
		SupportsSubSlicing:     false,
		RequiresWorkloadPolicy: true,
	}
	userFacingNameToSystemCharacteristics["gpu-gb200-4-nolssd"] = SystemCharacteristics{
		Topology:               "1x72",
		VMsPerSlice:            1,
		GKEAccelerator:         "nvidia-gb200",
		GCEMachineType:         "a4x-highgpu-4g-nolssd",
		ChipsPerVM:             4,
		AcceleratorType:        AcceleratorTypeGPU,
		DeviceType:             "gpu-gb200-4",
		SupportsSubSlicing:     false,
		RequiresWorkloadPolicy: true,
	}

	// b200-8
	userFacingNameToSystemCharacteristics["gpu-b200-8"] = SystemCharacteristics{
		Topology:               "N/A",
		VMsPerSlice:            1,
		GKEAccelerator:         "nvidia-b200",
		GCEMachineType:         "a4-highgpu-8g",
		ChipsPerVM:             8,
		AcceleratorType:        AcceleratorTypeGPU,
		DeviceType:             "gpu-b200-8",
		SupportsSubSlicing:     false,
		RequiresWorkloadPolicy: true,
	}

	// h200-141gb-8
	userFacingNameToSystemCharacteristics["gpu-h200-141gb-8"] = SystemCharacteristics{
		Topology:               "N/A",
		VMsPerSlice:            1,
		GKEAccelerator:         "nvidia-h200-141gb",
		GCEMachineType:         "a3-ultragpu-8g",
		ChipsPerVM:             8,
		AcceleratorType:        AcceleratorTypeGPU,
		DeviceType:             "gpu-h200-141gb-8",
		SupportsSubSlicing:     false,
		RequiresWorkloadPolicy: true,
	}

	// h100-80gb-8
	userFacingNameToSystemCharacteristics["gpu-h100-80gb-8"] = SystemCharacteristics{
		Topology:               "N/A",
		VMsPerSlice:            1,
		GKEAccelerator:         "nvidia-h100-80gb",
		GCEMachineType:         "a3-highgpu-8g",
		ChipsPerVM:             8,
		AcceleratorType:        AcceleratorTypeGPU,
		DeviceType:             "gpu-h100-80gb-8",
		SupportsSubSlicing:     false,
		RequiresWorkloadPolicy: true,
	}

	// h100-mega-80gb-8
	userFacingNameToSystemCharacteristics["gpu-h100-mega-80gb-8"] = SystemCharacteristics{
		Topology:               "N/A",
		VMsPerSlice:            1,
		GKEAccelerator:         "nvidia-h100-mega-80gb",
		GCEMachineType:         "a3-megagpu-8g",
		ChipsPerVM:             8,
		AcceleratorType:        AcceleratorTypeGPU,
		DeviceType:             "gpu-h100-mega-80gb-8",
		SupportsSubSlicing:     false,
		RequiresWorkloadPolicy: true,
	}
}

func registerTPUCharacteristics() {
	// tpu7x
	mergeMap(getTPUSystemCharacteristicsMap(
		"tpu-7x", 2, "tpu7x", "tpu7x-standard-1t",
		[]string{"1x1x1"}, false, true, nil,
	))
	mergeMap(getTPUSystemCharacteristicsMap(
		"tpu-7x", 2, "tpu7x", "tpu7x-standard-4t",
		generateTPUTopologies(144, true), false, true,
		map[string]bool{
			"12x12x12": true, "12x12x16": true, "12x12x20": true, "12x12x24": true,
			"12x12x28": true, "12x12x36": true, "12x16x16": true, "12x16x20": true,
			"12x16x24": true, "12x16x28": true, "12x20x20": true, "12x20x24": true,
			"12x24x24": true, "16x16x16": true, "16x16x20": true, "16x16x24": true,
			"16x16x32": true, "16x20x28": true, "16x24x24": true, "2x2x1": true,
			"2x2x2": true, "2x2x4": true, "2x4x4": true, "4x12x116": true,
			"4x12x12": true, "4x12x124": true, "4x12x20": true, "4x12x28": true,
			"4x12x44": true, "4x12x52": true, "4x12x68": true, "4x12x76": true,
			"4x12x92": true, "4x20x20": true, "4x20x28": true, "4x20x44": true,
			"4x20x52": true, "4x20x68": true, "4x20x76": true, "4x28x28": true,
			"4x28x44": true, "4x28x52": true, "4x4x116": true, "4x4x12": true,
			"4x4x124": true, "4x4x148": true, "4x4x164": true, "4x4x172": true,
			"4x4x188": true, "4x4x20": true, "4x4x212": true, "4x4x236": true,
			"4x4x244": true, "4x4x28": true, "4x4x4": true, "4x4x44": true,
			"4x4x52": true, "4x4x68": true, "4x4x76": true, "4x4x8": true,
			"4x4x92": true, "4x8x116": true, "4x8x12": true, "4x8x124": true,
			"4x8x148": true, "4x8x164": true, "4x8x172": true, "4x8x188": true,
			"4x8x20": true, "4x8x28": true, "4x8x44": true, "4x8x52": true,
			"4x8x68": true, "4x8x76": true, "4x8x8": true, "4x8x92": true,
			"8x12x12": true, "8x12x16": true, "8x12x20": true, "8x12x28": true,
			"8x12x44": true, "8x12x52": true, "8x16x16": true, "8x16x20": true,
			"8x16x28": true, "8x16x44": true, "8x20x20": true, "8x20x28": true,
			"8x8x12": true, "8x8x16": true, "8x8x20": true, "8x8x28": true,
			"8x8x44": true, "8x8x52": true, "8x8x68": true, "8x8x76": true,
			"8x8x8": true, "8x8x92": true,
		},
	))

	// v6e
	mergeMap(getTPUSystemCharacteristicsMap(
		"tpu-v6e", 1, "tpu-v6e-slice", "ct6e-standard-1t",
		[]string{"1x1"}, false, false, nil,
	))
	mergeMap(getTPUSystemCharacteristicsMap(
		"tpu-v6e", 1, "tpu-v6e-slice", "ct6e-standard-4t",
		[]string{"2x2"}, false, false, nil,
	))
	mergeMap(getTPUSystemCharacteristicsMap(
		"tpu-v6e", 1, "tpu-v6e-slice", "ct6e-standard-4t",
		[]string{"2x4", "4x4", "4x8", "8x8", "8x16", "16x16"}, true, false, nil,
	))

	// v5p
	mergeMap(getTPUSystemCharacteristicsMap(
		"tpu-v5p", 2, "tpu-v5p-slice", "ct5p-hightpu-4t",
		generateTPUTopologies(140, true), false, false,
		map[string]bool{
			"2x2x1": true, "2x2x2": true, "2x2x4": true, "2x4x4": true, "4x4x4": true,
			"4x4x8": true, "4x4x12": true, "4x8x8": true, "4x4x20": true, "4x8x12": true,
			"4x4x28": true, "8x8x8": true, "4x12x12": true, "4x8x20": true, "4x4x44": true,
			"8x8x12": true, "4x4x52": true, "4x8x28": true, "4x12x20": true, "8x8x16": true,
			"4x4x68": true, "8x12x12": true, "4x4x76": true, "8x8x20": true, "4x12x28": true,
			"4x8x44": true, "4x4x92": true, "8x12x16": true, "4x20x20": true, "4x8x52": true,
			"12x12x12": true, "8x8x28": true, "4x4x116": true, "8x12x20": true, "4x4x124": true,
			"8x16x16": true, "4x12x44": true, "4x8x68": true, "4x20x28": true, "12x12x16": true,
			"4x4x148": true, "4x8x76": true, "4x12x52": true, "8x16x20": true, "4x4x164": true,
			"8x12x28": true, "4x4x172": true, "8x8x44": true, "12x12x20": true, "4x8x92": true,
			"4x4x188": true, "12x16x16": true, "4x28x28": true, "8x20x20": true, "4x12x68": true,
			"8x8x52": true, "4x4x212": true, "12x12x24": true, "4x20x44": true, "8x16x28": true,
			"4x12x76": true, "4x8x116": true, "4x4x236": true, "12x16x20": true, "4x4x244": true,
			"4x8x124": true, "12x12x28": true, "16x16x16": true, "4x20x52": true, "8x12x44": true,
			"8x8x68": true, "4x12x92": true, "8x20x28": true, "12x16x24": true, "4x8x148": true,
			"12x20x20": true, "8x8x76": true, "4x28x44": true, "8x12x52": true, "16x16x20": true,
			"12x12x36": true, "4x8x164": true, "12x16x28": true, "4x20x68": true, "4x8x172": true,
			"4x12x116": true, "8x16x44": true, "12x20x24": true, "4x28x52": true, "8x8x92": true,
			"4x12x124": true, "4x8x188": true, "4x20x76": true, "16x16x24": true, "12x24x24": true,
			"16x20x28": true,
		},
	))

	// v5litepod
	mergeMap(getTPUSystemCharacteristicsMap(
		"tpu-v5litepod", 1, "tpu-v5-lite-podslice", "ct5lp-hightpu-4t",
		[]string{"2x4", "4x4", "4x8", "8x8", "8x16", "16x16"}, false, false, nil,
	))

	// v4
	mergeMap(getTPUSystemCharacteristicsMap(
		"tpu-v4", 2, "tpu-v4-podslice", "ct4p-hightpu-4t",
		generateTPUTopologies(64, false), false, false,
		map[string]bool{
			"2x2x1": true, "2x2x2": true, "2x2x4": true, "2x4x4": true, "4x4x4": true,
			"4x4x8": true, "4x8x8": true, "8x8x8": true, "8x8x12": true, "8x8x16": true,
			"8x16x16": true,
		},
	))
}

// Helper functions

func mergeMap(source map[string]SystemCharacteristics) {
	for k, v := range source {
		userFacingNameToSystemCharacteristics[k] = v
	}
}

func getTPUSystemCharacteristicsMap(
	prefix string,
	tensorcoresPerChip int,
	gkeAccelerator string,
	machineType string,
	supportedTopologies []string,
	supportsSubSlicing bool,
	tpuTypeRequiresWorkloadPolicy bool,
	defaultTopologies map[string]bool,
) map[string]SystemCharacteristics {
	systemCharacteristicsMap := make(map[string]SystemCharacteristics)
	if defaultTopologies == nil {
		defaultTopologies = make(map[string]bool)
	}

	for _, topology := range supportedTopologies {
		chipsPerVM := computeChipsPerVM(topology)
		vmsPerSlice := computeVMsPerSlice(topology)
		numTensorcores := computeNumTensorcores(tensorcoresPerChip, topology)
		deviceType := fmt.Sprintf("%s-%d", prefix, numTensorcores)

		requiresWorkloadPolicy := tpuTypeRequiresWorkloadPolicy && vmsPerSlice > 1

		system := SystemCharacteristics{
			Topology:               topology,
			VMsPerSlice:            vmsPerSlice,
			GKEAccelerator:         gkeAccelerator,
			GCEMachineType:         machineType,
			ChipsPerVM:             chipsPerVM,
			AcceleratorType:        AcceleratorTypeTPU,
			DeviceType:             deviceType,
			RequiresWorkloadPolicy: requiresWorkloadPolicy,
			SupportsSubSlicing:     supportsSubSlicing,
		}

		systemCharacteristicsMap[fmt.Sprintf("%s-%s", prefix, topology)] = system

		if defaultTopologies[topology] || systemCharacteristicsMap[deviceType].DeviceType == "" {
			systemCharacteristicsMap[deviceType] = system
		}
	}
	return systemCharacteristicsMap
}

func generateTPUTopologies(maxCubes int, enforceNondecreasing bool) []string {
	topologies := []string{"2x2x1", "2x2x2", "2x2x4", "2x4x4"}
	max := 256
	for x := 4; x <= max; x += 4 {
		yStart := 4
		if enforceNondecreasing {
			yStart = x
		}
		for y := yStart; y <= max; y += 4 {
			zStart := 4
			if enforceNondecreasing {
				zStart = y
			}
			for z := zStart; z <= max; z += 4 {
				if (x/4)*(y/4)*(z/4) <= maxCubes {
					topologies = append(topologies, fmt.Sprintf("%dx%dx%d", x, y, z))
				}
			}
		}
	}
	return topologies
}

func computeChipsPerVM(topology string) int {
	if getTopologyProduct(topology) == 1 {
		return 1
	}
	return 4
}

func computeNumTensorcores(tensorcoresPerChip int, topology string) int {
	return getTopologyProduct(topology) * tensorcoresPerChip
}

func computeVMsPerSlice(topology string) int {
	chipsPerVM := computeChipsPerVM(topology)
	return getTopologyProduct(topology) / chipsPerVM
}

func getTopologyProduct(topology string) int {
	parts := strings.Split(topology, "x")
	product := 1
	for _, part := range parts {
		val, _ := strconv.Atoi(part)
		product *= val
	}
	return product
}

// GetSystemCharacteristics returns the system characteristics for a given device type.
func GetSystemCharacteristics(deviceType string) (*SystemCharacteristics, error) {
	if val, ok := userFacingNameToSystemCharacteristics[deviceType]; ok {
		return &val, nil
	}
	return nil, fmt.Errorf("unknown device type: %s", deviceType)
}
