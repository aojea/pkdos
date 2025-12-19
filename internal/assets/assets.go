package assets

import (
	_ "embed"
	"fmt"
	"runtime"
)

//go:embed krun-agent-fsync-amd64
var agentFsyncBinaryAmd64 []byte

//go:embed krun-agent-fsync-arm64
var agentFsyncBinaryArm64 []byte

func GetAgentFsyncBinaryForArch() ([]byte, error) {
	arch := runtime.GOARCH

	switch arch {
	case "amd64":
		return agentFsyncBinaryAmd64, nil
	case "arm64":
		return agentFsyncBinaryArm64, nil
	default:
		return nil, fmt.Errorf("unsupported architecture: %s", arch)
	}
}
