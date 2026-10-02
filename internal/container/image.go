package container

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// pullImage fetches image (a "repo/name:tag"-style reference) into the
// local OCI-layout cache under cacheDir via skopeo.
func pullImage(cacheDir, image string, cred *RegistryCredential) error {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return fmt.Errorf("image: creating cache dir: %w", err)
	}
	ociLayoutDir := filepath.Join(cacheDir, sanitizeCacheName(image))
	umociImage := ociLayoutDir + ":cached"
	skopeoRef := "oci:" + umociImage

	args := []string{"copy"}
	if cred != nil {
		args = append(args, "--src-creds", cred.Username+":"+cred.Token)
	}
	args = append(args, "docker://"+image, skopeoRef)

	cmd := exec.Command("skopeo", args...)
	cmd.Env = append(os.Environ(), "HOME=/run/nodexa")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("skopeo copy %s: %w: %s", image, err, strings.TrimSpace(string(out)))
	}
	if err := writeImageMeta(ociLayoutDir, image); err != nil {
		return fmt.Errorf("image: writing cache metadata for %s: %w", image, err)
	}
	return nil
}

// unpackImage unpacks a cached OCI-layout image into a fresh bundleDir via umoci.
func unpackImage(cacheDir, bundleDir, image string) error {
	ociLayoutDir := filepath.Join(cacheDir, sanitizeCacheName(image))
	umociImage := ociLayoutDir + ":cached"

	if err := os.RemoveAll(bundleDir); err != nil {
		return fmt.Errorf("clearing old bundle %s: %w", bundleDir, err)
	}
	if out, err := exec.Command("umoci", "unpack", "--image", umociImage, bundleDir).CombinedOutput(); err != nil {
		return fmt.Errorf("umoci unpack %s: %w: %s", image, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// pullAndUnpack fetches image into the local cache and unpacks it into bundleDir.
func pullAndUnpack(cacheDir, bundleDir, image string, cred *RegistryCredential) error {
	if err := pullImage(cacheDir, image, cred); err != nil {
		return err
	}
	return unpackImage(cacheDir, bundleDir, image)
}

// sanitizeCacheName turns an arbitrary image reference into a safe
// filesystem path component.
func sanitizeCacheName(image string) string {
	var b strings.Builder
	for _, r := range image {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	return b.String()
}

// writeBundleConfig rewrites the config.json umoci unpack generated from
// the image's own OCI config, applying spec's process args/env, mounts,
// volumes, cgroupsPath, and network mode.
// netnsOpt can contain: [netnsPath, containerIP, gatewayIP].
func writeBundleConfig(bundleDir string, spec ServiceSpec, volumesDir string, netnsOpt ...string) error {
	var netnsPath, containerIP, gatewayIP string
	if len(netnsOpt) > 0 {
		netnsPath = netnsOpt[0]
	}
	if len(netnsOpt) > 1 {
		containerIP = netnsOpt[1]
	}
	if len(netnsOpt) > 2 {
		gatewayIP = netnsOpt[2]
	}

	path := filepath.Join(bundleDir, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}

	process, _ := cfg["process"].(map[string]any)
	if process == nil {
		return fmt.Errorf("%s: missing \"process\"", path)
	}
	// umoci unpack's default config.json sets terminal: true (a tty for
	// interactive use), but runc.go's Run always launches via `runc run -d`
	// (detached, no --console-socket) -- runc refuses to allocate a tty
	// without one, so a detached container needs terminal: false.
	process["terminal"] = false
	if len(spec.Command) > 0 {
		process["args"] = toAnySlice(spec.Command)
	}
	if len(spec.Env) > 0 {
		process["env"] = mergeEnv(process["env"], spec.Env)
	}

	defaultCaps := []any{
		"CAP_CHOWN",
		"CAP_DAC_OVERRIDE",
		"CAP_FSETID",
		"CAP_FOWNER",
		"CAP_MKNOD",
		"CAP_NET_RAW",
		"CAP_SETGID",
		"CAP_SETUID",
		"CAP_SETFCAP",
		"CAP_SETPCAP",
		"CAP_NET_BIND_SERVICE",
		"CAP_SYS_CHROOT",
		"CAP_KILL",
		"CAP_AUDIT_WRITE",
	}
	caps, _ := process["capabilities"].(map[string]any)
	if caps == nil {
		caps = map[string]any{}
		process["capabilities"] = caps
	}
	for _, set := range []string{"bounding", "effective", "permitted", "inheritable", "ambient"} {
		caps[set] = defaultCaps
	}

	linux, _ := cfg["linux"].(map[string]any)
	if linux == nil {
		linux = map[string]any{}
		cfg["linux"] = linux
	}
	linux["cgroupsPath"] = "nodexa/" + spec.Name

	networkMode := spec.Network
	if networkMode == "" {
		networkMode = "bridge"
	}

	if networkMode == "host" {
		namespaces, _ := linux["namespaces"].([]any)
		linux["namespaces"] = removeNamespace(namespaces, "network")
	} else if networkMode == "none" {
		namespaces, _ := linux["namespaces"].([]any)
		linux["namespaces"] = ensureNamespace(namespaces, "network", "")
	} else {
		namespaces, _ := linux["namespaces"].([]any)
		linux["namespaces"] = ensureNamespace(namespaces, "network", netnsPath)
	}

	// Hostname in OCI spec and rootfs
	cfg["hostname"] = spec.Name
	etcDir := filepath.Join(bundleDir, "rootfs", "etc")
	_ = os.MkdirAll(etcDir, 0o755)
	_ = os.WriteFile(filepath.Join(etcDir, "hostname"), []byte(spec.Name+"\n"), 0o644)

	// Ensure DNS resolv.conf exists in bundle rootfs
	resolvPath := filepath.Join(etcDir, "resolv.conf")
	if networkMode != "host" {
		gw := gatewayIP
		if gw == "" {
			gw = "172.17.0.1"
		}
		resolvContent := fmt.Sprintf("nameserver %s\nnameserver 1.1.1.1\nnameserver 8.8.8.8\n", gw)
		_ = os.WriteFile(resolvPath, []byte(resolvContent), 0o644)
	} else {
		resolvData, err := os.ReadFile("/etc/resolv.conf")
		if err != nil || len(resolvData) == 0 {
			resolvData = []byte("nameserver 1.1.1.1\nnameserver 8.8.8.8\n")
		}
		_ = os.WriteFile(resolvPath, resolvData, 0o644)
	}

	// Ensure /etc/hosts has local loopback and container's own name
	hostsPath := filepath.Join(etcDir, "hosts")
	var hostsContent string
	if networkMode == "host" {
		hostsContent = "127.0.0.1 localhost localhost.localdomain\n::1 localhost localhost.localdomain\n"
	} else {
		ip := containerIP
		if ip == "" {
			ip = "127.0.0.1"
		}
		hostsContent = fmt.Sprintf("127.0.0.1 localhost localhost.localdomain\n::1 localhost localhost.localdomain\n\n%s %s", ip, spec.Name)
		if spec.ContainerName != "" && spec.ContainerName != spec.Name {
			hostsContent += " " + spec.ContainerName
		}
		hostsContent += "\n"
	}
	_ = os.WriteFile(hostsPath, []byte(hostsContent), 0o644)

	mounts, _ := cfg["mounts"].([]any)
	for _, mnt := range spec.Mounts {
		mode := "rw"
		if mnt.ReadOnly {
			mode = "ro"
		}
		mounts = append(mounts, map[string]any{
			"destination": mnt.ContainerPath,
			"type":        "bind",
			"source":      mnt.HostPath,
			"options":     []any{"bind", mode},
		})
	}
	for _, vol := range spec.Volumes {
		hostPath := filepath.Join(volumesDir, vol.Name)
		if err := os.MkdirAll(hostPath, 0o755); err != nil {
			return fmt.Errorf("creating volume dir %s: %w", hostPath, err)
		}
		mounts = append(mounts, map[string]any{
			"destination": vol.ContainerPath,
			"type":        "bind",
			"source":      hostPath,
			"options":     []any{"bind", "rw"},
		})
	}
	cfg["mounts"] = mounts

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// removeNamespace filters namespaces (config.json's linux.namespaces) down
// to entries whose "type" isn't nsType, dropping it in place.
func removeNamespace(namespaces []any, nsType string) []any {
	out := namespaces[:0]
	for _, ns := range namespaces {
		if m, ok := ns.(map[string]any); ok && m["type"] == nsType {
			continue
		}
		out = append(out, ns)
	}
	return out
}

// ensureNamespace ensures a namespace entry of nsType exists in namespaces,
// configuring its "path" if provided.
func ensureNamespace(namespaces []any, nsType, path string) []any {
	found := false
	out := make([]any, 0, len(namespaces)+1)
	for _, ns := range namespaces {
		if m, ok := ns.(map[string]any); ok && m["type"] == nsType {
			entry := map[string]any{"type": nsType}
			if path != "" {
				entry["path"] = path
			}
			out = append(out, entry)
			found = true
			continue
		}
		out = append(out, ns)
	}
	if !found {
		entry := map[string]any{"type": nsType}
		if path != "" {
			entry["path"] = path
		}
		out = append(out, entry)
	}
	return out
}

// mergeEnv returns the OCI process.env list with vars set, replacing any
// existing entries of the same names.
func mergeEnv(current any, vars map[string]string) []any {
	env, _ := current.([]any)
	var merged []any
	for _, e := range env {
		str, ok := e.(string)
		if !ok {
			continue
		}
		parts := strings.SplitN(str, "=", 2)
		if _, exists := vars[parts[0]]; !exists {
			merged = append(merged, e)
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		merged = append(merged, k+"="+vars[k])
	}
	return merged
}

// applyEnv sets vars in bundleDir's config.json, rewriting it only when
// something changed.
func applyEnv(bundleDir string, vars map[string]string) error {
	if len(vars) == 0 {
		return nil
	}
	path := filepath.Join(bundleDir, "config.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parsing %s: %w", path, err)
	}
	process, _ := cfg["process"].(map[string]any)
	if process == nil {
		return fmt.Errorf("%s: missing \"process\"", path)
	}
	have := map[string]bool{}
	if env, ok := process["env"].([]any); ok {
		for _, e := range env {
			if s, ok := e.(string); ok {
				have[s] = true
			}
		}
	}
	changed := false
	for k, v := range vars {
		if !have[k+"="+v] {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}
	process["env"] = mergeEnv(process["env"], vars)
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func toAnySlice(items []string) []any {
	out := make([]any, len(items))
	for i, s := range items {
		out[i] = s
	}
	return out
}
