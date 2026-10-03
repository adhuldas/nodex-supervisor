package container

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// EngineType represents the container runtime engine in use.
type EngineType string

const (
	EngineRunc    EngineType = "runc"
	EngineDocker  EngineType = "docker"
	EngineNerdctl EngineType = "nerdctl"
)

// IsDockerAvailable checks whether the docker CLI is present in PATH and the daemon is responding.
func IsDockerAvailable() bool {
	if _, err := exec.LookPath("docker"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "docker", "info", "--format", "{{.ServerVersion}}")
	return cmd.Run() == nil
}

// IsNerdctlAvailable checks whether the nerdctl CLI is present in PATH and responsive.
func IsNerdctlAvailable() bool {
	if _, err := exec.LookPath("nerdctl"); err != nil {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "nerdctl", "info")
	return cmd.Run() == nil
}

// cliBinary returns the executable name for the chosen engine.
func (e EngineType) cliBinary() string {
	if e == EngineNerdctl {
		return "nerdctl"
	}
	return "docker"
}

// dockerPull pulls a container image using docker or nerdctl CLI.
func dockerPull(ctx context.Context, cli, image string, cred *RegistryCredential) error {
	if cred != nil && cred.Username != "" && cred.Token != "" {
		parts := strings.Split(image, "/")
		if len(parts) > 1 && strings.Contains(parts[0], ".") {
			registry := parts[0]
			loginCmd := exec.CommandContext(ctx, cli, "login", registry, "-u", cred.Username, "--password-stdin")
			loginCmd.Stdin = strings.NewReader(cred.Token)
			_ = loginCmd.Run()
		}
	}

	cmd := exec.CommandContext(ctx, cli, "pull", image)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s pull %s: %w: %s", cli, image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerCreate creates a container according to ServiceSpec using docker or nerdctl CLI.
func dockerCreate(ctx context.Context, cli string, spec ServiceSpec, deploymentName string, deploymentRevision int, deviceEnv map[string]string) error {
	name := spec.Name
	_ = exec.CommandContext(ctx, cli, "rm", "-f", name).Run()

	args := []string{"create", "--name", name}

	if spec.Restart != "" {
		args = append(args, "--restart="+spec.Restart)
	} else {
		args = append(args, "--restart=unless-stopped")
	}

	if spec.Network != "" {
		args = append(args, "--network="+spec.Network)
	}

	for k, v := range spec.Env {
		args = append(args, "-e", k+"="+v)
	}
	for k, v := range deviceEnv {
		args = append(args, "-e", k+"="+v)
	}

	for _, p := range spec.Ports {
		portArg := ""
		if p.HostPort != 0 {
			portArg += fmt.Sprintf("%d:", p.HostPort)
		}
		portArg += fmt.Sprintf("%d", p.ContainerPort)
		if p.Protocol != "" {
			portArg += "/" + p.Protocol
		}
		args = append(args, "-p", portArg)
	}

	for _, m := range spec.Mounts {
		ro := ""
		if m.ReadOnly {
			ro = ":ro"
		}
		args = append(args, "-v", fmt.Sprintf("%s:%s%s", m.HostPath, m.ContainerPath, ro))
	}

	for _, v := range spec.Volumes {
		args = append(args, "-v", fmt.Sprintf("%s:%s", v.Name, v.ContainerPath))
	}

	args = append(args,
		"--label", "nodexa.managed=true",
		"--label", "nodexa.service="+name,
		"--label", fmt.Sprintf("nodexa.deployment.name=%s", deploymentName),
		"--label", fmt.Sprintf("nodexa.deployment.revision=%d", deploymentRevision),
	)

	args = append(args, spec.Image)
	args = append(args, spec.Command...)

	cmd := exec.CommandContext(ctx, cli, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s create %s: %w: %s", cli, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerStart starts an existing container by name.
func dockerStart(ctx context.Context, cli, name string) error {
	cmd := exec.CommandContext(ctx, cli, "start", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s start %s: %w: %s", cli, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerStop stops a running container by name.
func dockerStop(ctx context.Context, cli, name string) error {
	cmd := exec.CommandContext(ctx, cli, "stop", "-t", "10", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s stop %s: %w: %s", cli, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerRemove stops and removes a container by name.
func dockerRemove(ctx context.Context, cli, name string) error {
	cmd := exec.CommandContext(ctx, cli, "rm", "-f", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s rm %s: %w: %s", cli, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// dockerList lists all containers on the engine formatted as NodexaContainer.
func dockerList(ctx context.Context, cli, containerDir string) ([]NodexaContainer, error) {
	cmd := exec.CommandContext(ctx, cli, "ps", "-a", "--format", "{{json .}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s ps: %w", cli, err)
	}

	var list []NodexaContainer
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry struct {
			ID        string `json:"ID"`
			Names     string `json:"Names"`
			Image     string `json:"Image"`
			State     string `json:"State"`
			Labels    string `json:"Labels"`
			CreatedAt string `json:"CreatedAt"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			continue
		}

		cleanName := strings.TrimPrefix(strings.Split(entry.Names, ",")[0], "/")
		c := NodexaContainer{
			Name:      cleanName,
			Image:     entry.Image,
			State:     dockerState(entry.State),
			CreatedAt: parseDockerTime(entry.CreatedAt),
			External:  !hasLabel(entry.Labels, "nodexa.managed=true"),
		}

		// Read disk metadata if available for deployment details
		if containerDir != "" && !c.External {
			if md, _ := readMetadata(filepath.Join(containerDir, cleanName)); md != nil {
				c.DeploymentName = md.DeploymentName
				c.DeploymentRevision = md.DeploymentRevision
				c.CreatedAt = md.CreatedAt
			}
		}

		list = append(list, c)
	}

	return list, nil
}

// dockerIsExternal reports whether a container named name exists and
// wasn't created by this supervisor (no nodexa.managed label).
func dockerIsExternal(ctx context.Context, cli, name string) bool {
	out, err := exec.CommandContext(ctx, cli, "inspect", "--type", "container",
		"--format", `{{index .Config.Labels "nodexa.managed"}}`, name).Output()
	if err != nil {
		return false // no such container
	}
	return strings.TrimSpace(string(out)) != "true"
}

// dockerImageExists reports whether image is already on the host.
func dockerImageExists(ctx context.Context, cli, image string) bool {
	return exec.CommandContext(ctx, cli, "image", "inspect", image).Run() == nil
}

// dockerState maps Docker's container states onto the supervisor's own
// vocabulary. nodexa-backend rejects the whole heartbeat over any state
// outside it, so states only Docker has can't pass through as-is.
func dockerState(s string) State {
	switch strings.ToLower(s) {
	case "running":
		return StateRunning
	case "created":
		return StateCreated
	case "dead", "restarting":
		// restarting: crash-looping under a restart policy.
		return StateFailed
	default:
		// exited, paused, removing.
		return StateStopped
	}
}

// parseDockerTime parses `docker ps`'s CreatedAt ("2026-10-03 09:28:01
// +0000 UTC"); zero when it can't.
func parseDockerTime(s string) time.Time {
	t, err := time.Parse("2006-01-02 15:04:05 -0700 MST", s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

// hasLabel reports whether `docker ps`'s comma-separated Labels contain
// the exact key=value pair.
func hasLabel(labels, pair string) bool {
	for _, l := range strings.Split(labels, ",") {
		if strings.TrimSpace(l) == pair {
			return true
		}
	}
	return false
}

// dockerInspect inspects a container and returns detailed NodexaContainer info.
func dockerInspect(ctx context.Context, cli, name, containerDir string) (*NodexaContainer, error) {
	cmd := exec.CommandContext(ctx, cli, "inspect", name)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s inspect %s: %w", cli, name, err)
	}

	var inspectResults []struct {
		Id    string `json:"Id"`
		Name  string `json:"Name"`
		State struct {
			Status    string    `json:"Status"`
			Running   bool      `json:"Running"`
			Pid       int       `json:"Pid"`
			StartedAt time.Time `json:"StartedAt"`
		} `json:"State"`
		Created time.Time `json:"Created"`
		Config  struct {
			Image  string            `json:"Image"`
			Cmd    []string          `json:"Cmd"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			IPAddress string `json:"IPAddress"`
			Networks  map[string]struct {
				IPAddress string `json:"IPAddress"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}

	if err := json.Unmarshal(out, &inspectResults); err != nil || len(inspectResults) == 0 {
		return nil, fmt.Errorf("parsing %s inspect output: %w", cli, err)
	}

	res := inspectResults[0]
	var st State = StateStopped
	if res.State.Running {
		st = StateRunning
	}

	cleanName := strings.TrimPrefix(res.Name, "/")
	ip := res.NetworkSettings.IPAddress
	if ip == "" {
		for _, netInfo := range res.NetworkSettings.Networks {
			if netInfo.IPAddress != "" {
				ip = netInfo.IPAddress
				break
			}
		}
	}

	c := &NodexaContainer{
		Name:      cleanName,
		Image:     res.Config.Image,
		Command:   res.Config.Cmd,
		State:     st,
		PID:       res.State.Pid,
		CreatedAt: res.Created,
		StartedAt: res.State.StartedAt,
		IPAddress: ip,
	}

	if md, _ := readMetadata(filepath.Join(containerDir, cleanName)); md != nil {
		c.DeploymentName = md.DeploymentName
		c.DeploymentRevision = md.DeploymentRevision
	} else if res.Config.Labels != nil {
		c.DeploymentName = res.Config.Labels["nodexa.deployment.name"]
		rev, _ := strconv.Atoi(res.Config.Labels["nodexa.deployment.revision"])
		c.DeploymentRevision = rev
	}

	return c, nil
}

// dockerLogs retrieves logs from a container using docker or nerdctl CLI.
func dockerLogs(ctx context.Context, cli, name string, tail int) (string, error) {
	args := []string{"logs"}
	if tail > 0 {
		args = append(args, "--tail", fmt.Sprint(tail))
	}
	args = append(args, name)

	cmd := exec.CommandContext(ctx, cli, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s logs %s: %w: %s", cli, name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// dockerExec executes a command inside a running container.
func dockerExec(ctx context.Context, cli, name string, cmdArgs []string, user, cwd string) (string, string, int, error) {
	args := []string{"exec"}
	if user != "" {
		args = append(args, "-u", user)
	}
	if cwd != "" {
		args = append(args, "-w", cwd)
	}
	args = append(args, name)
	args = append(args, cmdArgs...)

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, cli, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	return stdout.String(), stderr.String(), exitCode, nil
}

// dockerStats fetches current resource usage for a container.
func dockerStats(ctx context.Context, cli, name string) (NodexaContainerStats, error) {
	cmd := exec.CommandContext(ctx, cli, "stats", "--no-stream", "--format", "{{json .}}", name)
	out, err := cmd.Output()
	if err != nil {
		return NodexaContainerStats{Name: name, Timestamp: time.Now().UTC()}, nil
	}

	var st struct {
		CPUPerc  string `json:"CPUPerc"`
		MemUsage string `json:"MemUsage"`
		PIDs     string `json:"PIDs"`
	}
	_ = json.Unmarshal(out, &st)

	pids, _ := strconv.Atoi(st.PIDs)
	var memUsage, memLimit uint64
	if parts := strings.Split(st.MemUsage, "/"); len(parts) == 2 {
		memUsage = uint64(parseDockerSize(parts[0]))
		memLimit = uint64(parseDockerSize(parts[1]))
	}

	return NodexaContainerStats{
		Name:             name,
		MemoryUsageBytes: memUsage,
		MemoryLimitBytes: memLimit,
		PIDs:             pids,
		Timestamp:        time.Now().UTC(),
	}, nil
}

func parseDockerSize(s string) int64 {
	s = strings.TrimSpace(s)
	multiplier := int64(1)
	upper := strings.ToUpper(s)
	if strings.HasSuffix(upper, "GB") || strings.HasSuffix(upper, "GIB") {
		multiplier = 1024 * 1024 * 1024
		s = strings.TrimRight(s, "gGbBiI")
	} else if strings.HasSuffix(upper, "MB") || strings.HasSuffix(upper, "MIB") {
		multiplier = 1024 * 1024
		s = strings.TrimRight(s, "mMbBiI")
	} else if strings.HasSuffix(upper, "KB") || strings.HasSuffix(upper, "KIB") {
		multiplier = 1024
		s = strings.TrimRight(s, "kKbBiI")
	} else if strings.HasSuffix(upper, "B") {
		s = strings.TrimSuffix(s, "B")
	}
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return int64(f * float64(multiplier))
}

// dockerImages lists images using docker or nerdctl CLI.
func dockerImages(ctx context.Context, cli string) ([]ImageInfo, error) {
	cmd := exec.CommandContext(ctx, cli, "images", "--format", "{{json .}}")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("%s images: %w", cli, err)
	}

	var images []ImageInfo
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var item struct {
			Repository string `json:"Repository"`
			Tag        string `json:"Tag"`
			Size       string `json:"Size"`
			CreatedAt  string `json:"CreatedAt"`
			ID         string `json:"ID"`
		}
		if err := json.Unmarshal([]byte(line), &item); err != nil {
			continue
		}
		name := item.Repository
		if item.Tag != "" && item.Tag != "<none>" {
			name += ":" + item.Tag
		}
		if name == "" || name == "<none>" {
			name = item.ID
		}

		createdAt := time.Now()
		parts := strings.Split(item.CreatedAt, " ")
		if len(parts) >= 2 {
			if t, err := time.Parse("2006-01-02 15:04:05", parts[0]+" "+parts[1]); err == nil {
				createdAt = t
			}
		}

		images = append(images, ImageInfo{
			Name:      name,
			SizeBytes: parseDockerSize(item.Size),
			PulledAt:  createdAt,
		})
	}

	sort.Slice(images, func(i, j int) bool { return images[i].PulledAt.After(images[j].PulledAt) })
	return images, nil
}

// dockerRemoveImage removes an image using docker or nerdctl CLI.
func dockerRemoveImage(ctx context.Context, cli, name string) error {
	cmd := exec.CommandContext(ctx, cli, "rmi", name)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s rmi %s: %w: %s", cli, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}
