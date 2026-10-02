package container

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ociSpecFragment captures only the OCI runtime-spec (config.json) fields
// Nodexa Agent needs to read back out. We deliberately do not depend on the
// full opencontainers/runtime-spec module for this: bundles are authored
// ahead of time (by a Yocto recipe or a future Nodexa Deploy pipeline), not
// generated on the fly by the agent in this phase, so a full spec writer is
// unnecessary surface area for a trusted, minimal-dependency component.
type ociSpecFragment struct {
	Process struct {
		Args []string `json:"args"`
	} `json:"process"`
	Linux struct {
		CgroupsPath string `json:"cgroupsPath"`
	} `json:"linux"`
}

// readBundleSpec reads the subset of config.json Nodexa Agent needs.
func readBundleSpec(bundleDir string) (*ociSpecFragment, error) {
	path := filepath.Join(bundleDir, "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	var spec ociSpecFragment
	if err := json.Unmarshal(b, &spec); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if spec.Linux.CgroupsPath == "" {
		return nil, fmt.Errorf("%s: linux.cgroupsPath is required by Nodexa container convention (expected \"nodexa/<name>\")", path)
	}
	return &spec, nil
}
