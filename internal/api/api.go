// Package api implements Nodexa Agent's local control API, exposed only
// over a Unix domain socket at /run/nodexa/agent.sock. It is never bound to
// TCP: nodexactl is the only intended client, and it must run on the same
// device.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/deploy"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/health"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/identity"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/state"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/system"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/update"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/version"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/wifi"
)

// Deps are the subsystems the API surfaces. Handlers only ever read from
// these; nothing about system state, identity, or backend trust can be
// mutated through this API in Phase 1 -- it is observability plus basic
// container lifecycle control only.
type Deps struct {
	Identity    *identity.Identity
	HealthCheck *health.Checker
	Containers  *container.NodexaContainerManager
	Networks    *container.NetworkManager
	Deploy      *deploy.Manager
	Update      *update.AgentUpdater
	State       *state.Store
	Events      *events.Bus
	SocketGroup string
	VPNIP       func() string
}

// Server is the local control API server.
type Server struct {
	deps     Deps
	socket   string
	listener net.Listener
	http     *http.Server
}

// New creates an API server bound to socketPath (not yet listening).
func New(socketPath string, deps Deps) *Server {
	mux := http.NewServeMux()
	s := &Server{deps: deps, socket: socketPath, http: &http.Server{Handler: mux}}

	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("GET /v1/version", s.handleVersion)
	mux.HandleFunc("GET /v1/identity", s.handleIdentity)
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/containers", s.handleContainers)
	mux.HandleFunc("GET /v1/containers/{name}", s.handleContainerInspect)
	mux.HandleFunc("GET /v1/containers/{name}/stats", s.handleContainerStats)
	mux.HandleFunc("GET /v1/containers/{name}/logs", s.handleContainerLogs)
	mux.HandleFunc("POST /v1/containers/{name}/start", s.handleContainerStart)
	mux.HandleFunc("POST /v1/containers/{name}/stop", s.handleContainerStop)
	mux.HandleFunc("POST /v1/containers/{name}/exec", s.handleContainerExec)
	mux.HandleFunc("DELETE /v1/containers/{name}", s.handleContainerRemove)
	mux.HandleFunc("GET /v1/images", s.handleImages)
	// {name...} (not {name}): an image reference can itself contain "/"
	// (e.g. "ghcr.io/org/repo:tag"), which a plain {name} segment can't
	// match -- {name...} is net/http's wildcard-to-end-of-path pattern.
	mux.HandleFunc("DELETE /v1/images/{name...}", s.handleImageRemove)
	mux.HandleFunc("GET /v1/deployment", s.handleDeployment)
	mux.HandleFunc("GET /v1/network", s.handleNetwork)
	mux.HandleFunc("GET /v1/networks", s.handleNetworksList)
	mux.HandleFunc("POST /v1/networks", s.handleNetworkCreate)
	mux.HandleFunc("DELETE /v1/networks/{name}", s.handleNetworkRemove)
	mux.HandleFunc("GET /v1/networks/{name}", s.handleNetworkInspect)
	mux.HandleFunc("GET /v1/network/wifi/networks", s.handleWifiNetworks)
	mux.HandleFunc("GET /v1/wifi/networks", s.handleWifiNetworks)
	mux.HandleFunc("POST /v1/network/wifi", s.handleWifiChange)
	mux.HandleFunc("POST /v1/wifi", s.handleWifiChange)
	mux.HandleFunc("GET /v1/update/status", s.handleUpdateStatus)
	mux.HandleFunc("POST /v1/update/apply", s.handleUpdateApply)
	mux.HandleFunc("POST /v1/update/rollback", s.handleUpdateRollback)

	return s
}

// ListenAndServe creates the Unix socket with restrictive permissions and
// serves the API until the listener is closed.
//
// The socket is:
//   - created fresh (any stale socket file from a previous run is removed)
//   - mode 0660: read/write for owner (nodexa) and group (nodexa) only
//   - group-owned by deps.SocketGroup, so nodexactl works for any user in
//     that group without needing to run as the nodexa user itself
//   - never bound to TCP
func (s *Server) ListenAndServe() error {
	_ = os.Remove(s.socket)

	if err := os.MkdirAll(dirOf(s.socket), 0o755); err != nil {
		return fmt.Errorf("api: creating run dir: %w", err)
	}

	l, err := net.Listen("unix", s.socket)
	if err != nil {
		return fmt.Errorf("api: listening on %s: %w", s.socket, err)
	}
	s.listener = l

	if err := os.Chmod(s.socket, 0o660); err != nil {
		return fmt.Errorf("api: chmod socket: %w", err)
	}
	if err := chownToGroup(s.socket, s.deps.SocketGroup); err != nil {
		// The configured group (typically "nodexa") doesn't exist on this
		// host -- common on third-party devices (someone's Mac, a random
		// Linux box). Without widening the mode, the socket stays
		// root:root 0660 and only root can reach the API, blocking every
		// UI / nodexactl call that isn't over Tailscale SSH as root.
		s.deps.Events.Emit(events.HealthCheck, "socket group not found, falling back to world-accessible socket", events.Fieldsf("error", "%v", err))
		if chmodErr := os.Chmod(s.socket, 0o666); chmodErr != nil {
			s.deps.Events.Emit(events.HealthCheck, "could not widen socket permissions", events.Fieldsf("error", "%v", chmodErr))
		}
	}

	return s.http.Serve(l)
}

// Close shuts down the API server and removes the socket file.
func (s *Server) Close() error {
	if s.listener != nil {
		_ = s.listener.Close()
	}
	_ = os.Remove(s.socket)
	return nil
}

func dirOf(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' {
			return path[:i]
		}
	}
	return "."
}

func chownToGroup(path, groupName string) error {
	if groupName == "" {
		return nil
	}
	g, err := user.LookupGroup(groupName)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	return os.Chown(path, -1, gid)
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// --- handlers ---

// StatusResponse is the shape returned by GET /v1/status, matching the
// fields `nodexactl status` renders.
type StatusResponse struct {
	DeviceID     string        `json:"device_id"`
	OSVersion    string        `json:"os_version"`
	AgentVersion string        `json:"agent_version"`
	Architecture string        `json:"architecture"`
	Uptime       time.Duration `json:"uptime_seconds"`
	UptimeHuman  string        `json:"uptime"`
	Agent        health.Status `json:"agent"`
	Runtime      health.Status `json:"runtime"`
	Network      health.Status `json:"network"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	sys := system.Collect()
	h := s.deps.HealthCheck.Check()

	writeJSON(w, StatusResponse{
		DeviceID:     s.deps.Identity.DeviceID,
		OSVersion:    update.RunningOSVersion(),
		AgentVersion: version.AgentVersion,
		Architecture: sys.Architecture,
		Uptime:       sys.Uptime,
		UptimeHuman:  system.FormatUptime(sys.Uptime),
		Agent:        h.Agent,
		Runtime:      h.Runtime,
		Network:      h.Network,
	})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, version.Info{
		OSVersion:    update.RunningOSVersion(),
		AgentVersion: version.AgentVersion,
		Commit:       version.Commit,
		BuildDate:    version.BuildDate,
		Architecture: system.Collect().Architecture,
	})
}

func (s *Server) handleIdentity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.deps.Identity)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.deps.HealthCheck.Check())
}

func (s *Server) handleContainers(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.Containers.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, list)
}

func (s *Server) handleContainerInspect(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	ctr, err := s.deps.Containers.Get(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, ctr)
}

func (s *Server) handleContainerStats(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	stats, err := s.deps.Containers.Stats(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, stats)
}

// ContainerLogsResponse is the shape returned by
// GET /v1/containers/{name}/logs.
type ContainerLogsResponse struct {
	Name string `json:"name"`
	Log  string `json:"log"`
}

func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	tail := 0 // 0 means "whole file" - see NodexaContainerManager.Logs
	if v := r.URL.Query().Get("tail"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid tail value %q", v))
			return
		}
		tail = n
	}

	if r.URL.Query().Get("follow") == "true" {
		s.handleContainerLogsFollow(w, r, name, tail)
		return
	}

	log, err := s.deps.Containers.Logs(name, tail)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, ContainerLogsResponse{Name: name, Log: log})
}

// logFollowPollInterval is how often handleContainerLogsFollow checks the
// log file for new output. Polling rather than an inotify/fsnotify watch:
// nodexa-agent's do_compile[network]="0" means dependencies are vendored
// (see the nodexa-agent recipe's GOFLAGS=-mod=vendor comment), and
// container.Stats already accepts the same "poll periodically" trade-off
// reading cgroup files rather than pulling in a watch dependency for it.
const logFollowPollInterval = 300 * time.Millisecond

// handleContainerLogsFollow streams name's log file to w as plain text:
// its last tail lines (or the whole file if tail is 0), then newly
// appended output as it's written, until the client disconnects. This is
// deliberately a separate response shape from the non-follow path's JSON
// envelope -- a streamed response has no single well-formed body to wrap
// in one -- so nodexactl's client treats follow as plain text, not JSON.
func (s *Server) handleContainerLogsFollow(w http.ResponseWriter, r *http.Request, name string, tail int) {
	// Docker/nerdctl: use the engine's native `docker logs --follow` which
	// is context-aware and cleans up immediately when the client disconnects,
	// instead of the runc file-polling path whose accumulated goroutines and
	// open fds from rapid container switching in the UI starved tailscaled
	// on resource-constrained third-party devices.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("streaming unsupported"))
		return
	}

	handled, err := s.deps.Containers.LogFollow(r.Context(), name, tail, w)
	if err != nil {
		// Only report errors if headers haven't been sent yet; once
		// streaming starts, a broken pipe is expected on disconnect.
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if handled {
		return // Docker/nerdctl native follow completed (or client disconnected)
	}

	// runc engine: fall through to the existing file-polling path.
	path, err := s.deps.Containers.LogFilePath(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	var offset int64
	if f, err := os.Open(path); err == nil {
		if tail > 0 {
			b, _ := io.ReadAll(f)
			io.WriteString(w, container.LastLines(string(b), tail))
			offset = int64(len(b))
		} else {
			n, _ := io.Copy(w, f)
			offset = n
		}
		f.Close()
		flusher.Flush()
	} else if !os.IsNotExist(err) {
		return // headers already sent; nothing more we can usefully report
	}

	ticker := time.NewTicker(logFollowPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			f, err := os.Open(path)
			if err != nil {
				continue // not created yet (container never started, or no output written)
			}
			fi, err := f.Stat()
			if err != nil || fi.Size() <= offset {
				f.Close()
				continue
			}
			if _, err := f.Seek(offset, io.SeekStart); err != nil {
				f.Close()
				continue
			}
			n, _ := io.Copy(w, f)
			f.Close()
			offset += n
			flusher.Flush()
		}
	}
}

func (s *Server) handleContainerStart(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.deps.Containers.Start(name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleContainerStop(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.deps.Containers.Stop(name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ExecRequest is the payload for POST /v1/containers/{name}/exec.
type ExecRequest struct {
	Command []string `json:"command"`
	User    string   `json:"user,omitempty"`
	Workdir string   `json:"workdir,omitempty"`
}

// ExecResponse is the response from POST /v1/containers/{name}/exec.
type ExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

func (s *Server) handleContainerExec(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req ExecRequest
	if r.Body != nil && r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("parsing request body: %w", err))
			return
		}
	}
	if len(req.Command) == 0 {
		req.Command = []string{"/bin/sh"}
	}
	stdout, stderr, exitCode, err := s.deps.Containers.Exec(name, req.Command, req.User, req.Workdir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, ExecResponse{
		ExitCode: exitCode,
		Stdout:   stdout,
		Stderr:   stderr,
	})
}

func (s *Server) handleContainerRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.deps.Containers.Remove(name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	list, err := s.deps.Containers.Images()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, list)
}

func (s *Server) handleImageRemove(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.deps.Containers.RemoveImage(name); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// DeploymentStatusResponse is the shape returned by GET /v1/deployment,
// matching what `nodexa deploy status` renders: the last Nodexa Deploy
// deployment this device applied, and its services' current container
// state (an empty name/0 revision means no deployment has been applied).
type DeploymentStatusResponse struct {
	DeploymentName  string                      `json:"deployment_name,omitempty"`
	AppliedRevision int                         `json:"applied_revision"`
	Services        []container.NodexaContainer `json:"services"`
}

func (s *Server) handleDeployment(w http.ResponseWriter, r *http.Request) {
	name := s.deps.Deploy.AppliedName()

	list, err := s.deps.Containers.List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	var services []container.NodexaContainer
	for _, c := range list {
		if c.DeploymentName == name {
			services = append(services, c)
		}
	}

	writeJSON(w, DeploymentStatusResponse{
		DeploymentName:  name,
		AppliedRevision: s.deps.Deploy.AppliedRevision(),
		Services:        services,
	})
}

// NetworkInterface describes a physical or virtual network interface on the device.
type NetworkInterface struct {
	Name        string   `json:"name"`
	State       string   `json:"state"`
	MAC         string   `json:"mac,omitempty"`
	IPAddresses []string `json:"ip_addresses"`
	Flags       []string `json:"flags,omitempty"`
}

// NetworkResponse is the shape returned by GET /v1/network.
type NetworkResponse struct {
	Status      string             `json:"status"`
	TailscaleIP string             `json:"tailscale_ip,omitempty"`
	VPNStatus   string             `json:"vpn_status"`
	Interfaces  []NetworkInterface `json:"interfaces"`
}

func (s *Server) handleNetwork(w http.ResponseWriter, r *http.Request) {
	ifaces, err := net.Interfaces()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	var list []NetworkInterface
	hasUp := false

	for _, iface := range ifaces {
		state := "DOWN"
		if iface.Flags&net.FlagUp != 0 {
			state = "UP"
		}

		var flags []string
		if iface.Flags&net.FlagUp != 0 {
			flags = append(flags, "UP")
		}
		if iface.Flags&net.FlagBroadcast != 0 {
			flags = append(flags, "BROADCAST")
		}
		if iface.Flags&net.FlagLoopback != 0 {
			flags = append(flags, "LOOPBACK")
		}
		if iface.Flags&net.FlagPointToPoint != 0 {
			flags = append(flags, "POINTOPOINT")
		}
		if iface.Flags&net.FlagMulticast != 0 {
			flags = append(flags, "MULTICAST")
		}

		var ipAddrs []string
		if addrs, err := iface.Addrs(); err == nil {
			for _, a := range addrs {
				ipAddrs = append(ipAddrs, a.String())
			}
		}

		if iface.Flags&net.FlagLoopback == 0 && state == "UP" && len(ipAddrs) > 0 {
			hasUp = true
		}

		list = append(list, NetworkInterface{
			Name:        iface.Name,
			State:       state,
			MAC:         iface.HardwareAddr.String(),
			IPAddresses: ipAddrs,
			Flags:       flags,
		})
	}

	var vpnIP string
	if s.deps.VPNIP != nil {
		vpnIP = s.deps.VPNIP()
	}
	vpnStatus := "disconnected"
	if vpnIP != "" {
		vpnStatus = "connected"
	}

	status := "offline"
	if hasUp {
		status = "online"
	}

	writeJSON(w, NetworkResponse{
		Status:      status,
		TailscaleIP: vpnIP,
		VPNStatus:   vpnStatus,
		Interfaces:  list,
	})
}

func (s *Server) handleNetworksList(w http.ResponseWriter, r *http.Request) {
	if s.deps.Networks == nil {
		writeJSON(w, []container.ContainerNetwork{})
		return
	}
	writeJSON(w, s.deps.Networks.List())
}

func (s *Server) handleNetworkCreate(w http.ResponseWriter, r *http.Request) {
	if s.deps.Networks == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("network manager not initialized"))
		return
	}

	var req container.CreateNetworkRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json request: %w", err))
		return
	}

	netObj, err := s.deps.Networks.Create(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(netObj)
}

func (s *Server) handleNetworkRemove(w http.ResponseWriter, r *http.Request) {
	if s.deps.Networks == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("network manager not initialized"))
		return
	}

	name := r.PathValue("name")
	if name == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("network name is required"))
		return
	}

	if err := s.deps.Networks.Remove(name); err != nil {
		if strings.Contains(err.Error(), "not found") {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleNetworkInspect(w http.ResponseWriter, r *http.Request) {
	if s.deps.Networks == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("network manager not initialized"))
		return
	}

	name := r.PathValue("name")
	netObj, err := s.deps.Networks.Get(name)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	writeJSON(w, netObj)
}

// UpdateApplyRequest is the JSON payload for POST /v1/update/apply.
type UpdateApplyRequest struct {
	URL     string `json:"url,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Version string `json:"version,omitempty"`
	Path    string `json:"path,omitempty"`
}

func (s *Server) handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	if s.deps.Update == nil {
		writeJSON(w, update.AgentStatus{
			CurrentVersion: version.AgentVersion,
			RunningBinary:  "unknown",
		})
		return
	}
	writeJSON(w, s.deps.Update.Status())
}

func (s *Server) handleUpdateApply(w http.ResponseWriter, r *http.Request) {
	if s.deps.Update == nil {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("update engine not configured"))
		return
	}
	var req UpdateApplyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	if req.URL != "" {
		go func() {
			_ = s.deps.Update.ApplyFromURL(context.Background(), backend.AgentUpdateTarget{
				Version: req.Version,
				URL:     req.URL,
				SHA256:  req.SHA256,
			}, nil)
		}()
		writeJSON(w, map[string]string{"status": "update started"})
		return
	}

	if req.Path != "" {
		if err := s.deps.Update.ApplyFromFile(r.Context(), req.Path, req.SHA256, req.Version); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]string{"status": "update applied successfully"})
		return
	}

	writeError(w, http.StatusBadRequest, fmt.Errorf("url or path must be specified"))
}

func (s *Server) handleUpdateRollback(w http.ResponseWriter, r *http.Request) {
	if s.deps.Update == nil {
		writeError(w, http.StatusNotImplemented, fmt.Errorf("update engine not configured"))
		return
	}
	if err := s.deps.Update.Rollback(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, map[string]string{"status": "rolled back to base binary"})
}

// WifiChangeRequest is the body for POST /v1/network/wifi and POST /v1/wifi.
type WifiChangeRequest struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
}

// WifiChangeResponse is returned after applying Wi-Fi changes.
type WifiChangeResponse struct {
	Status string `json:"status"`
	SSID   string `json:"ssid"`
}

func (s *Server) handleWifiNetworks(w http.ResponseWriter, r *http.Request) {
	nets, err := wifi.Scan(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if nets == nil {
		nets = []wifi.WifiNetwork{}
	}
	writeJSON(w, nets)
}

func (s *Server) handleWifiChange(w http.ResponseWriter, r *http.Request) {
	var req WifiChangeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid json request: %w", err))
		return
	}
	creds := wifi.Credentials{
		SSID:     req.SSID,
		Password: req.Password,
	}
	if err := creds.Validate(); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := wifi.Change(r.Context(), creds); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if s.deps.Events != nil {
		s.deps.Events.Emit(events.NetworkReady, "wifi network changed via api", events.Fieldsf("ssid", "%s", creds.SSID))
	}
	writeJSON(w, WifiChangeResponse{
		Status: "applied",
		SSID:   creds.SSID,
	})
}


