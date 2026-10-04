// Package client is nodexactl's HTTP-over-Unix-socket client for talking to
// nodexa-agent's local control API at /run/nodexa/agent.sock.
//
// nodexactl never inspects system state directly (no reading
// /var/lib/nodexa files, no shelling out to runc, etc.) -- every command
// goes through nodexa-agent, which remains the single trusted source of
// truth for the device. This also means nodexactl and nodexa-agent can
// evolve independently: the API's /v1/ prefix is nodexa-agent's versioned
// contract, not an internal implementation detail nodexctl reaches past.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/config"
)

// DefaultSocketPath is where nodexa-agent listens by default.
const DefaultSocketPath = config.DefaultSocketPath

// Client talks to nodexa-agent over its Unix domain socket.
type Client struct {
	socketPath string
	http       *http.Client
	// streamHTTP is used only by ContainerLogsFollow: a long-running follow
	// stream has no natural fixed duration, so it deliberately has no
	// http.Client.Timeout (unlike http above) -- the caller's ctx, not a
	// deadline here, is what ends it.
	streamHTTP *http.Client
}

// New creates a Client bound to socketPath.
func New(socketPath string) *Client {
	dialer := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", socketPath)
	}
	return &Client{
		socketPath: socketPath,
		http: &http.Client{
			Timeout:   5 * time.Second,
			Transport: &http.Transport{DialContext: dialer},
		},
		streamHTTP: &http.Client{
			Transport: &http.Transport{DialContext: dialer},
		},
	}
}

// StatusResponse mirrors nodexa-agent's GET /v1/status response.
type StatusResponse struct {
	DeviceID     string `json:"device_id"`
	OSVersion    string `json:"os_version"`
	AgentVersion string `json:"agent_version"`
	Architecture string `json:"architecture"`
	UptimeHuman  string `json:"uptime"`
	Agent        string `json:"agent"`
	Runtime      string `json:"runtime"`
	Network      string `json:"network"`
}

// VersionResponse mirrors nodexa-agent's GET /v1/version response.
type VersionResponse struct {
	OSVersion    string `json:"os_version"`
	AgentVersion string `json:"agent_version"`
	Commit       string `json:"build_commit"`
	BuildDate    string `json:"build_date"`
	Architecture string `json:"architecture"`
}

// IdentityResponse mirrors nodexa-agent's GET /v1/identity response.
type IdentityResponse struct {
	DeviceID    string   `json:"device_id"`
	Fingerprint string   `json:"hardware_fingerprint"`
	Provider    string   `json:"provider"`
	SourceKeys  []string `json:"source_keys"`
	CreatedAt   string   `json:"created_at"`
}

// HealthResponse mirrors nodexa-agent's GET /v1/health response.
type HealthResponse struct {
	Overall         string  `json:"overall"`
	Agent           string  `json:"agent"`
	Runtime         string  `json:"runtime"`
	Network         string  `json:"network"`
	LoadAvg1        float64 `json:"load_avg_1m"`
	MemUsedPercent  float64 `json:"mem_used_percent"`
	DiskUsedPercent float64 `json:"disk_used_percent"`
	MemTotalBytes   uint64  `json:"mem_total_bytes"`
	MemUsedBytes    uint64  `json:"mem_used_bytes"`
	DiskTotalBytes  uint64  `json:"disk_total_bytes"`
	DiskUsedBytes   uint64  `json:"disk_used_bytes"`
	Timestamp       string  `json:"timestamp"`
}

// ContainerResponse mirrors one entry of nodexa-agent's GET /v1/containers
// response.
type ContainerResponse struct {
	Name               string   `json:"name"`
	Image              string   `json:"image"`
	Command            []string `json:"command"`
	State              string   `json:"state"`
	PID                int      `json:"pid,omitempty"`
	CreatedAt          string   `json:"created_at"`
	StartedAt          string   `json:"started_at,omitempty"`
	DeploymentName     string   `json:"deployment_name,omitempty"`
	DeploymentRevision int      `json:"deployment_revision,omitempty"`
	Network            string   `json:"network,omitempty"`
	IPAddress          string   `json:"ip_address,omitempty"`
}

// ImageResponse mirrors one entry of nodexa-agent's GET /v1/images
// response.
type ImageResponse struct {
	Name      string    `json:"name"`
	SizeBytes int64     `json:"size_bytes"`
	PulledAt  time.Time `json:"pulled_at"`
}

// ContainerStatsResponse mirrors nodexa-agent's per-container stats
// response.
type ContainerStatsResponse struct {
	Name             string  `json:"name"`
	CPUUsageSeconds  float64 `json:"cpu_usage_seconds"`
	MemoryUsageBytes uint64  `json:"memory_usage_bytes"`
	MemoryLimitBytes uint64  `json:"memory_limit_bytes,omitempty"`
	PIDs             int     `json:"pids"`
	Timestamp        string  `json:"timestamp"`
}

func (c *Client) Status(ctx context.Context) (*StatusResponse, error) {
	var v StatusResponse
	if err := c.get(ctx, "/v1/status", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) Version(ctx context.Context) (*VersionResponse, error) {
	var v VersionResponse
	if err := c.get(ctx, "/v1/version", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// NetworkInterface mirrors one entry of nodexa-agent's GET /v1/network response.
type NetworkInterface struct {
	Name        string   `json:"name"`
	State       string   `json:"state"`
	MAC         string   `json:"mac,omitempty"`
	IPAddresses []string `json:"ip_addresses"`
	Flags       []string `json:"flags,omitempty"`
}

// NetworkResponse mirrors nodexa-agent's GET /v1/network response.
type NetworkResponse struct {
	Status      string             `json:"status"`
	TailscaleIP string             `json:"tailscale_ip,omitempty"`
	VPNStatus   string             `json:"vpn_status"`
	Interfaces  []NetworkInterface `json:"interfaces"`
}

func (c *Client) Network(ctx context.Context) (*NetworkResponse, error) {
	var v NetworkResponse
	if err := c.get(ctx, "/v1/network", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ContainerNetworkInfo mirrors nodexa-agent's GET /v1/networks entry.
type ContainerNetworkInfo struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Driver    string    `json:"driver"`
	Scope     string    `json:"scope"`
	Subnet    string    `json:"subnet,omitempty"`
	Gateway   string    `json:"gateway,omitempty"`
	Bridge    string    `json:"bridge,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	Internal  bool      `json:"internal,omitempty"`
}

// CreateNetworkRequest is the payload sent to POST /v1/networks.
type CreateNetworkRequest struct {
	Name    string `json:"name"`
	Driver  string `json:"driver,omitempty"`
	Subnet  string `json:"subnet,omitempty"`
	Gateway string `json:"gateway,omitempty"`
}

func (c *Client) NetworksList(ctx context.Context) ([]ContainerNetworkInfo, error) {
	var v []ContainerNetworkInfo
	if err := c.get(ctx, "/v1/networks", &v); err != nil {
		return nil, err
	}
	return v, nil
}

func (c *Client) NetworkCreate(ctx context.Context, req CreateNetworkRequest) (*ContainerNetworkInfo, error) {
	var v ContainerNetworkInfo
	if err := c.postJSON(ctx, "/v1/networks", req, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) NetworkRemove(ctx context.Context, name string) error {
	return c.noBody(ctx, http.MethodDelete, "/v1/networks/"+name)
}

func (c *Client) NetworkInspect(ctx context.Context, name string) (*ContainerNetworkInfo, error) {
	var v ContainerNetworkInfo
	if err := c.get(ctx, "/v1/networks/"+name, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) Identity(ctx context.Context) (*IdentityResponse, error) {
	var v IdentityResponse
	if err := c.get(ctx, "/v1/identity", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) Health(ctx context.Context) (*HealthResponse, error) {
	var v HealthResponse
	if err := c.get(ctx, "/v1/health", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) Containers(ctx context.Context) ([]ContainerResponse, error) {
	var v []ContainerResponse
	if err := c.get(ctx, "/v1/containers", &v); err != nil {
		return nil, err
	}
	return v, nil
}

func (c *Client) ContainerInspect(ctx context.Context, name string) (*ContainerResponse, error) {
	var v ContainerResponse
	if err := c.get(ctx, "/v1/containers/"+name, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func (c *Client) ContainerStats(ctx context.Context, name string) (*ContainerStatsResponse, error) {
	var v ContainerStatsResponse
	if err := c.get(ctx, "/v1/containers/"+name+"/stats", &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ContainerLogsResponse mirrors nodexa-agent's per-container logs response.
type ContainerLogsResponse struct {
	Name string `json:"name"`
	Log  string `json:"log"`
}

// ContainerLogs fetches a container's captured stdout/stderr. tail, if
// positive, limits the response to that many trailing lines.
func (c *Client) ContainerLogs(ctx context.Context, name string, tail int) (*ContainerLogsResponse, error) {
	path := "/v1/containers/" + name + "/logs"
	if tail > 0 {
		path += "?tail=" + strconv.Itoa(tail)
	}
	var v ContainerLogsResponse
	if err := c.get(ctx, path, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// ContainerLogsFollow streams a container's captured stdout/stderr to w:
// its last tail lines (0 for the whole file), then newly written output as
// it arrives, until ctx is canceled or the agent closes the connection.
// Unlike ContainerLogs this is plain text, not JSON -- see
// internal/api's handleContainerLogsFollow -- and it deliberately bypasses
// c.http's request timeout (a follow stream is meant to run indefinitely;
// ctx, not a fixed deadline, is what should end it, e.g. on Ctrl+C).
func (c *Client) ContainerLogsFollow(ctx context.Context, name string, tail int, w io.Writer) error {
	path := "/v1/containers/" + name + "/logs?follow=true"
	if tail > 0 {
		path += "&tail=" + strconv.Itoa(tail)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://nodexa-agent"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.streamHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil // canceled (e.g. Ctrl+C) -- not a real connectivity failure
		}
		return c.connectionError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	_, err = io.Copy(w, resp.Body)
	if err != nil && ctx.Err() != nil {
		return nil // canceled (e.g. Ctrl+C) -- not a real failure
	}
	return err
}

func (c *Client) ContainerStart(ctx context.Context, name string) error {
	return c.post(ctx, "/v1/containers/"+name+"/start")
}

func (c *Client) ContainerStop(ctx context.Context, name string) error {
	return c.post(ctx, "/v1/containers/"+name+"/stop")
}

func (c *Client) ContainerRemove(ctx context.Context, name string) error {
	return c.noBody(ctx, http.MethodDelete, "/v1/containers/"+name)
}

// ContainerExecRequest is the payload for POST /v1/containers/{name}/exec.
type ContainerExecRequest struct {
	Command []string `json:"command"`
	User    string   `json:"user,omitempty"`
	Workdir string   `json:"workdir,omitempty"`
}

// ContainerExecResponse is the response from POST /v1/containers/{name}/exec.
type ContainerExecResponse struct {
	ExitCode int    `json:"exit_code"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

// ContainerExec runs a command in a running container via nodexa-agent.
func (c *Client) ContainerExec(ctx context.Context, name string, req ContainerExecRequest) (*ContainerExecResponse, error) {
	var resp ContainerExecResponse
	if err := c.postJSON(ctx, "/v1/containers/"+name+"/exec", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func (c *Client) Images(ctx context.Context) ([]ImageResponse, error) {
	var v []ImageResponse
	if err := c.get(ctx, "/v1/images", &v); err != nil {
		return nil, err
	}
	return v, nil
}

func (c *Client) ImageRemove(ctx context.Context, name string) error {
	return c.noBody(ctx, http.MethodDelete, "/v1/images/"+name)
}

func (c *Client) get(ctx context.Context, path string, out interface{}) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://nodexa-agent"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.connectionError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func (c *Client) post(ctx context.Context, path string) error {
	return c.noBody(ctx, http.MethodPost, path)
}

// noBody performs a request with no request or response body -- every
// container lifecycle action (start/stop/remove) is a bare method+path.
func (c *Client) noBody(ctx context.Context, method, path string) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://nodexa-agent"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return c.connectionError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	return nil
}

func (c *Client) connectionError(err error) error {
	return fmt.Errorf("cannot reach nodex-supervisor at %s: %w\n(is it running? try: systemctl status nodex-supervisor; it may need sudo)", c.socketPath, err)
}

func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(resp.Body)
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return fmt.Errorf("nodexa-agent: %s", e.Error)
	}
	return fmt.Errorf("nodexa-agent: unexpected status %s", resp.Status)
}

func (c *Client) postJSON(ctx context.Context, path string, in, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://nodexa-agent"+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return c.connectionError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return apiError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// AgentUpdateStatus mirrors nodexa-agent's GET /v1/update/status response.
type AgentUpdateStatus struct {
	CurrentVersion    string `json:"current_version"`
	RunningBinary     string `json:"running_binary"`
	IsOTAActive       bool   `json:"is_ota_active"`
	OTAPresent        bool   `json:"ota_present"`
	RollbackAvailable bool   `json:"rollback_available"`
}

// UpdateApplyRequest mirrors nodexa-agent's POST /v1/update/apply request.
type UpdateApplyRequest struct {
	URL     string `json:"url,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
	Version string `json:"version,omitempty"`
	Path    string `json:"path,omitempty"`
}

// UpdateStatus calls GET /v1/update/status.
func (c *Client) UpdateStatus(ctx context.Context) (*AgentUpdateStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://nodexa-agent/v1/update/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, c.connectionError(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return nil, apiError(resp)
	}

	var status AgentUpdateStatus
	if err := json.NewDecoder(resp.Body).Decode(&status); err != nil {
		return nil, fmt.Errorf("decode update status: %w", err)
	}
	return &status, nil
}

// UpdateApply calls POST /v1/update/apply.
func (c *Client) UpdateApply(ctx context.Context, updateReq UpdateApplyRequest) error {
	return c.postJSON(ctx, "/v1/update/apply", updateReq, nil)
}

// UpdateRollback calls POST /v1/update/rollback.
func (c *Client) UpdateRollback(ctx context.Context) error {
	return c.postJSON(ctx, "/v1/update/rollback", map[string]any{}, nil)
}

// WifiNetwork mirrors the agent's available Wi-Fi network representation.
type WifiNetwork struct {
	SSID          string `json:"ssid"`
	SignalPercent int    `json:"signal_percent"`
	Security      string `json:"security"`
}

// WifiChangeRequest is the body for changing the Wi-Fi network.
type WifiChangeRequest struct {
	SSID     string `json:"ssid"`
	Password string `json:"password"`
}

// WifiChangeResponse is the result of changing the Wi-Fi network.
type WifiChangeResponse struct {
	Status string `json:"status"`
	SSID   string `json:"ssid"`
}

// WifiNetworks calls GET /v1/network/wifi/networks.
func (c *Client) WifiNetworks(ctx context.Context) ([]WifiNetwork, error) {
	var v []WifiNetwork
	if err := c.get(ctx, "/v1/network/wifi/networks", &v); err != nil {
		return nil, err
	}
	return v, nil
}

// WifiChange calls POST /v1/network/wifi.
func (c *Client) WifiChange(ctx context.Context, req WifiChangeRequest) (*WifiChangeResponse, error) {
	var resp WifiChangeResponse
	if err := c.postJSON(ctx, "/v1/network/wifi", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
