package container

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func metadataPath(bundleDir string) string { return filepath.Join(bundleDir, "metadata.json") }

func readMetadata(bundleDir string) (*metadata, error) {
	b, err := os.ReadFile(metadataPath(bundleDir))
	if err != nil {
		return nil, err
	}
	var m metadata
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parsing metadata.json: %w", err)
	}
	return &m, nil
}

func writeMetadata(bundleDir string, m *metadata) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := metadataPath(bundleDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, metadataPath(bundleDir))
}
