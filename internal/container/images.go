package container

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ImageInfo describes one image cached locally under imageCacheDir -- what
// `nodexa images` lists.
type ImageInfo struct {
	Name      string    `json:"name"` // original reference, e.g. "nginx:alpine"
	SizeBytes int64     `json:"size_bytes"`
	PulledAt  time.Time `json:"pulled_at"`
}

// imageMeta is pullAndUnpack's sidecar record of the original image
// reference for a cache entry. sanitizeCacheName is lossy (every
// non-alphanumeric character collapses to '_'), so the cache directory name
// alone can't be turned back into a human-meaningful reference for
// `nodexa images`/`nodexa rmi`.
type imageMeta struct {
	Image    string    `json:"image"`
	PulledAt time.Time `json:"pulled_at"`
}

// writeImageMeta records image's original reference next to its oci-layout
// cache directory. Called by pullAndUnpack after a successful pull.
func writeImageMeta(ociLayoutDir, image string) error {
	meta := imageMeta{Image: image, PulledAt: time.Now()}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ociLayoutDir+".json", raw, 0o600)
}

// Images lists every image cached under imageCacheDir, most recently pulled
// first.
func (m *NodexaContainerManager) Images() ([]ImageInfo, error) {
	if m.Engine() == EngineDocker || m.Engine() == EngineNerdctl {
		return dockerImages(context.Background(), m.Engine().cliBinary())
	}

	entries, err := os.ReadDir(m.imageCacheDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("container: reading image cache dir: %w", err)
	}

	var images []ImageInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		meta, err := readImageMeta(filepath.Join(m.imageCacheDir, e.Name()))
		if err != nil {
			continue // stale/corrupt sidecar -- not this listing's problem
		}
		layoutDir := strings.TrimSuffix(filepath.Join(m.imageCacheDir, e.Name()), ".json")
		size, err := dirSize(layoutDir)
		if err != nil {
			continue // sidecar with no matching layout dir -- already removed
		}
		images = append(images, ImageInfo{Name: meta.Image, SizeBytes: size, PulledAt: meta.PulledAt})
	}

	sort.Slice(images, func(i, j int) bool { return images[i].PulledAt.After(images[j].PulledAt) })
	return images, nil
}

// RemoveImage deletes a cached image by its original reference (as shown by
// Images), freeing its oci-layout cache entry. It does not affect any
// already-unpacked container bundle -- those are independent copies under
// containerDir made at deploy time, not references into imageCacheDir.
func (m *NodexaContainerManager) RemoveImage(name string) error {
	if m.Engine() == EngineDocker || m.Engine() == EngineNerdctl {
		return dockerRemoveImage(context.Background(), m.Engine().cliBinary(), name)
	}

	layoutDir := filepath.Join(m.imageCacheDir, sanitizeCacheName(name))
	metaPath := layoutDir + ".json"

	if _, err := os.Stat(layoutDir); os.IsNotExist(err) {
		return fmt.Errorf("container: no cached image %q", name)
	}

	if err := os.RemoveAll(layoutDir); err != nil {
		return fmt.Errorf("container: removing cached image %q: %w", name, err)
	}
	if err := os.Remove(metaPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("container: removing cached image %q metadata: %w", name, err)
	}
	return nil
}

func readImageMeta(path string) (imageMeta, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return imageMeta{}, err
	}
	var meta imageMeta
	if err := json.Unmarshal(raw, &meta); err != nil {
		return imageMeta{}, err
	}
	return meta, nil
}

func dirSize(dir string) (int64, error) {
	var total int64
	err := filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// PruneUnusedImages removes all cached OCI layouts and metadata in imageCacheDir
// whose image references are not in the keepImages set.
func (m *NodexaContainerManager) PruneUnusedImages(keepImages map[string]bool) error {
	if eng := m.Engine(); eng == EngineDocker || eng == EngineNerdctl {
		return m.prunePulledImages(eng.cliBinary(), keepImages)
	}

	entries, err := os.ReadDir(m.imageCacheDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("container: pruning images: %w", err)
	}

	keepSanitized := make(map[string]bool, len(keepImages))
	for img := range keepImages {
		keepSanitized[sanitizeCacheName(img)] = true
	}

	for _, e := range entries {
		base := strings.TrimSuffix(e.Name(), ".json")
		if !keepSanitized[base] {
			_ = os.RemoveAll(filepath.Join(m.imageCacheDir, e.Name()))
		}
	}
	return nil
}

// pulledImagesFile records the Docker/nerdctl images pulled for
// deployments: the host's other images belong to whoever else runs
// containers on it, so pruning only ever considers these.
func (m *NodexaContainerManager) pulledImagesFile() string {
	return filepath.Join(m.imageCacheDir, "docker-pulled.json")
}

func (m *NodexaContainerManager) readPulledImages() []string {
	var images []string
	if data, err := os.ReadFile(m.pulledImagesFile()); err == nil {
		_ = json.Unmarshal(data, &images)
	}
	return images
}

func (m *NodexaContainerManager) writePulledImages(images []string) error {
	if err := os.MkdirAll(m.imageCacheDir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(images)
	if err != nil {
		return err
	}
	return os.WriteFile(m.pulledImagesFile(), data, 0o644)
}

func (m *NodexaContainerManager) recordPulledImage(image string) {
	m.pulledMu.Lock()
	defer m.pulledMu.Unlock()
	images := m.readPulledImages()
	for _, img := range images {
		if img == image {
			return
		}
	}
	_ = m.writePulledImages(append(images, image))
}

// prunePulledImages removes recorded images not in keepImages. `rmi`
// without -f refuses an image any container still uses, so one shared
// with a container outside the release stays; it's retried next prune.
func (m *NodexaContainerManager) prunePulledImages(cli string, keepImages map[string]bool) error {
	m.pulledMu.Lock()
	defer m.pulledMu.Unlock()
	var remaining []string
	for _, img := range m.readPulledImages() {
		if keepImages[img] {
			remaining = append(remaining, img)
			continue
		}
		if err := dockerRemoveImage(context.Background(), cli, img); err != nil {
			remaining = append(remaining, img)
		}
	}
	return m.writePulledImages(remaining)
}
