package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// VolumeUsage is one volume and the space it takes.
type VolumeUsage struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
}

// ImageUsage is one image and the space it takes. Name is the reference
// ("nginx:alpine"), or the image ID for a dangling image.
type ImageUsage struct {
	Name      string `json:"name"`
	ID        string `json:"id,omitempty"`
	SizeBytes int64  `json:"size_bytes"`
}

// LogUsage is one container's log file and the space it takes.
type LogUsage struct {
	Container string `json:"container"`
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// EngineUsage is what the container engine in use holds on disk: volumes,
// images (those a container is running, and dangling ones nothing
// references any more) and container logs. It is sent with the heartbeat so
// the dashboard can show it and offer to reclaim the space (the
// "prune_dangling_images" and "clear_container_logs" device actions).
type EngineUsage struct {
	// Engine is "docker", "nerdctl" or "runc" (the supervisor's own runtime
	// on top of containerd/runc).
	Engine      string    `json:"engine"`
	CollectedAt time.Time `json:"collected_at"`

	Volumes      []VolumeUsage `json:"volumes"`
	VolumesBytes int64         `json:"volumes_bytes"`

	RunningImages []ImageUsage `json:"running_images"`
	RunningBytes  int64        `json:"running_images_bytes"`

	DanglingImages []ImageUsage `json:"dangling_images"`
	DanglingBytes  int64        `json:"dangling_images_bytes"`

	Logs      []LogUsage `json:"logs"`
	LogsBytes int64      `json:"logs_bytes"`
}

// Usage measures the volumes, images and logs of the engine in use.
func (m *NodexaContainerManager) Usage(ctx context.Context) (*EngineUsage, error) {
	eng := m.Engine()
	u := &EngineUsage{Engine: string(eng), CollectedAt: time.Now().UTC()}
	var err error
	if eng == EngineDocker || eng == EngineNerdctl {
		err = dockerUsage(ctx, eng.cliBinary(), u)
	} else {
		err = m.runcUsage(u)
	}
	if err != nil {
		return nil, err
	}
	u.total()
	return u, nil
}

// total fills in the byte sums and gives every list a stable order.
func (u *EngineUsage) total() {
	sort.Slice(u.Volumes, func(i, j int) bool { return u.Volumes[i].SizeBytes > u.Volumes[j].SizeBytes })
	sort.Slice(u.RunningImages, func(i, j int) bool { return u.RunningImages[i].SizeBytes > u.RunningImages[j].SizeBytes })
	sort.Slice(u.DanglingImages, func(i, j int) bool { return u.DanglingImages[i].SizeBytes > u.DanglingImages[j].SizeBytes })
	sort.Slice(u.Logs, func(i, j int) bool { return u.Logs[i].SizeBytes > u.Logs[j].SizeBytes })
	for _, v := range u.Volumes {
		u.VolumesBytes += v.SizeBytes
	}
	for _, i := range u.RunningImages {
		u.RunningBytes += i.SizeBytes
	}
	for _, i := range u.DanglingImages {
		u.DanglingBytes += i.SizeBytes
	}
	for _, l := range u.Logs {
		u.LogsBytes += l.SizeBytes
	}
	// Non-nil, so the cloud sees "none" rather than "unknown".
	if u.Volumes == nil {
		u.Volumes = []VolumeUsage{}
	}
	if u.RunningImages == nil {
		u.RunningImages = []ImageUsage{}
	}
	if u.DanglingImages == nil {
		u.DanglingImages = []ImageUsage{}
	}
	if u.Logs == nil {
		u.Logs = []LogUsage{}
	}
}

// runcUsage measures the supervisor's own runtime: volumes under volumesDir,
// cached OCI images, and each bundle's container.log. Its images are only
// ever cached, never left dangling.
func (m *NodexaContainerManager) runcUsage(u *EngineUsage) error {
	if entries, err := os.ReadDir(m.volumesDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			size, _ := dirSize(filepath.Join(m.volumesDir, e.Name()))
			u.Volumes = append(u.Volumes, VolumeUsage{Name: e.Name(), SizeBytes: size})
		}
	}

	images, err := m.Images()
	if err != nil {
		return err
	}
	for _, img := range images {
		u.RunningImages = append(u.RunningImages, ImageUsage{Name: img.Name, SizeBytes: img.SizeBytes})
	}

	list, err := m.List()
	if err != nil {
		return err
	}
	for _, c := range list {
		path := filepath.Join(m.containerDir, c.Name, "container.log")
		if fi, err := os.Stat(path); err == nil {
			u.Logs = append(u.Logs, LogUsage{Container: c.Name, Path: path, SizeBytes: fi.Size()})
		}
	}
	return nil
}

// dockerUsage fills u from the docker or nerdctl CLI.
func dockerUsage(ctx context.Context, cli string, u *EngineUsage) error {
	vols, err := dockerVolumes(ctx, cli)
	if err != nil {
		return err
	}
	u.Volumes = vols

	running, err := cliLines(ctx, cli, "ps", "--format", "{{.Image}}")
	if err != nil {
		return err
	}
	inUse := make(map[string]bool, len(running))
	for _, r := range running {
		inUse[r] = true
	}

	all, err := dockerImageItems(ctx, cli, "images", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	for _, it := range all {
		name := it.name()
		if inUse[name] || inUse[it.ID] || (it.Repository != "" && inUse[it.Repository]) {
			u.RunningImages = append(u.RunningImages, ImageUsage{Name: name, ID: it.ID, SizeBytes: parseDockerSize(it.Size)})
		}
	}

	dangling, err := dockerImageItems(ctx, cli, "images", "-f", "dangling=true", "--format", "{{json .}}")
	if err != nil {
		return err
	}
	for _, it := range dangling {
		u.DanglingImages = append(u.DanglingImages, ImageUsage{Name: it.ID, ID: it.ID, SizeBytes: parseDockerSize(it.Size)})
	}

	logs, err := dockerLogUsage(ctx, cli)
	if err != nil {
		return err
	}
	u.Logs = logs
	return nil
}

type dockerImageItem struct {
	Repository string `json:"Repository"`
	Tag        string `json:"Tag"`
	ID         string `json:"ID"`
	Size       string `json:"Size"`
}

func (i dockerImageItem) name() string {
	if i.Repository == "" || i.Repository == "<none>" {
		return i.ID
	}
	if i.Tag == "" || i.Tag == "<none>" {
		return i.Repository
	}
	return i.Repository + ":" + i.Tag
}

func dockerImageItems(ctx context.Context, cli string, args ...string) ([]dockerImageItem, error) {
	lines, err := cliLines(ctx, cli, args...)
	if err != nil {
		return nil, err
	}
	var items []dockerImageItem
	for _, line := range lines {
		var it dockerImageItem
		if json.Unmarshal([]byte(line), &it) == nil {
			items = append(items, it)
		}
	}
	return items, nil
}

// dockerVolumes lists volumes with their sizes. Docker only reports sizes
// through `system df -v`; nerdctl through `volume ls --size`.
func dockerVolumes(ctx context.Context, cli string) ([]VolumeUsage, error) {
	if cli == "nerdctl" {
		lines, err := cliLines(ctx, cli, "volume", "ls", "--size", "--format", "{{json .}}")
		if err != nil {
			return nil, err
		}
		var vols []VolumeUsage
		for _, line := range lines {
			var v struct{ Name, Size string }
			if json.Unmarshal([]byte(line), &v) == nil && v.Name != "" {
				vols = append(vols, VolumeUsage{Name: v.Name, SizeBytes: parseDockerSize(v.Size)})
			}
		}
		return vols, nil
	}

	out, err := exec.CommandContext(ctx, cli, "system", "df", "-v", "--format", "{{json .}}").Output()
	if err != nil {
		return nil, fmt.Errorf("%s system df: %w", cli, err)
	}
	var df struct {
		Volumes []struct{ Name, Size string }
	}
	if err := json.Unmarshal(bytes.TrimSpace(out), &df); err != nil {
		return nil, fmt.Errorf("%s system df: %w", cli, err)
	}
	var vols []VolumeUsage
	for _, v := range df.Volumes {
		vols = append(vols, VolumeUsage{Name: v.Name, SizeBytes: parseDockerSize(v.Size)})
	}
	return vols, nil
}

// dockerLogUsage sizes every container's json-file log. Reading it needs
// the privileges the agent runs with; a log that can't be stat'd is skipped.
func dockerLogUsage(ctx context.Context, cli string) ([]LogUsage, error) {
	ids, err := cliLines(ctx, cli, "ps", "-aq")
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	args := append([]string{"inspect", "--format", "{{.Name}}\t{{.LogPath}}"}, ids...)
	lines, err := cliLines(ctx, cli, args...)
	if err != nil {
		return nil, err
	}
	var logs []LogUsage
	for _, line := range lines {
		name, path, ok := strings.Cut(line, "\t")
		path = strings.TrimSpace(path)
		if !ok || path == "" {
			continue
		}
		fi, err := os.Stat(path)
		if err != nil {
			continue
		}
		logs = append(logs, LogUsage{Container: strings.TrimPrefix(name, "/"), Path: path, SizeBytes: fi.Size()})
	}
	return logs, nil
}

// cliLines runs the CLI and returns its non-empty output lines.
func cliLines(ctx context.Context, cli string, args ...string) ([]string, error) {
	out, err := exec.CommandContext(ctx, cli, args...).Output()
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", cli, args[0], err)
	}
	var lines []string
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		if line := strings.TrimSpace(sc.Text()); line != "" {
			lines = append(lines, line)
		}
	}
	return lines, nil
}

// PruneDanglingImages removes images nothing references (docker/nerdctl
// only; the supervisor's own cache has none) and returns the bytes freed.
func (m *NodexaContainerManager) PruneDanglingImages(ctx context.Context) (int64, error) {
	eng := m.Engine()
	if eng != EngineDocker && eng != EngineNerdctl {
		return 0, nil
	}
	cli := eng.cliBinary()
	dangling, err := dockerImageItems(ctx, cli, "images", "-f", "dangling=true", "--format", "{{json .}}")
	if err != nil {
		return 0, err
	}
	var freed int64
	for _, it := range dangling {
		freed += parseDockerSize(it.Size)
	}
	if out, err := exec.CommandContext(ctx, cli, "image", "prune", "-f").CombinedOutput(); err != nil {
		return 0, fmt.Errorf("%s image prune: %w: %s", cli, err, strings.TrimSpace(string(out)))
	}
	return freed, nil
}

// ClearLogs truncates container's log file, or every container's when
// container is empty, and returns the bytes freed. Truncating (not
// deleting) keeps the file the engine and the running container hold open.
func (m *NodexaContainerManager) ClearLogs(ctx context.Context, container string) (int64, error) {
	u := &EngineUsage{}
	var err error
	if eng := m.Engine(); eng == EngineDocker || eng == EngineNerdctl {
		u.Logs, err = dockerLogUsage(ctx, eng.cliBinary())
	} else {
		err = m.runcUsage(u)
	}
	if err != nil {
		return 0, err
	}

	var freed int64
	var failed []string
	matched := false
	for _, l := range u.Logs {
		if container != "" && l.Container != container {
			continue
		}
		matched = true
		if err := os.Truncate(l.Path, 0); err != nil {
			failed = append(failed, fmt.Sprintf("%s: %v", l.Container, err))
			continue
		}
		freed += l.SizeBytes
	}
	if container != "" && !matched {
		return 0, fmt.Errorf("no log file for container %q", container)
	}
	if len(failed) > 0 {
		return freed, fmt.Errorf("clearing logs: %s", strings.Join(failed, "; "))
	}
	return freed, nil
}
