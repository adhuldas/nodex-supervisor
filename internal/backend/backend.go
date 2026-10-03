// Package backend is a small, honest Phase-1.5 bridge to nodexa-backend's
// device-facing API: plain HTTPS, a bearer token, talking directly to the
// backend named in a device's provisioning config.json (see
// internal/provisioning). It is not internal/transport's future trust
// model -- no mutual TLS, no pinned backend identity, no signed commands.
// That hardened channel is still Unimplemented and unrelated to this one;
// see docs/security.md for how the two are meant to coexist and eventually
// diverge (this bridge is expected to be superseded, not extended, once
// internal/transport lands).
//
// Request/response shapes mirror nodexa-backend's app/schemas/device.py
// (DeviceRegisterRequest/Response and DeviceHeartbeatRequest/Response) and
// app/schemas/deployment.py (DeviceDeploymentOut) exactly -- these are
// meant to be read side by side, not evolved independently.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
)

// defaultTimeout bounds every request this client makes. A slow or
// unreachable backend must never hang device boot or the health-report
// ticker -- registration and heartbeats are best-effort from the device's
// point of view.
const defaultTimeout = 15 * time.Second

// Client talks to one nodexa-backend instance. Its base URL can be
// switched at runtime (SetBaseURL) when the cloud URL is changed, so every
// holder of the same *Client follows along.
type Client struct {
	mu         sync.RWMutex
	baseURL    string
	httpClient *http.Client
}

// BaseURL returns the backend base URL requests currently go to.
func (c *Client) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// SetBaseURL points every later request at baseURL (trailing slashes
// trimmed, as in NewClient).
func (c *Client) SetBaseURL(baseURL string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = strings.TrimRight(baseURL, "/")
}

// NewClient creates a Client for the backend at baseURL (e.g.
// "https://backend.example.com"). baseURL is used as-is, trailing slashes
// trimmed; it comes from a device's provisioning config.json, not from a
// trusted, protected store, so callers should treat outages/misconfigured
// URLs as expected, non-fatal conditions.

// ResolveURL takes a URL or relative path and returns an absolute URL.
// If pathOrURL is already absolute (starts with http:// or https://), it is returned as-is.
// If it is a relative path (starts with "/"), it is joined with the client's baseURL scheme & host.
func (c *Client) ResolveURL(pathOrURL string) string {
	if strings.HasPrefix(pathOrURL, "http://") || strings.HasPrefix(pathOrURL, "https://") {
		return pathOrURL
	}
	if c == nil || c.BaseURL() == "" {
		return pathOrURL
	}
	base := c.BaseURL()
	parsed, err := url.Parse(base)
	if err == nil && parsed.Scheme != "" && parsed.Host != "" {
		if strings.HasPrefix(pathOrURL, "/") {
			return fmt.Sprintf("%s://%s%s", parsed.Scheme, parsed.Host, pathOrURL)
		}
		return fmt.Sprintf("%s/%s", base, pathOrURL)
	}
	if strings.HasPrefix(pathOrURL, "/") {
		return base + pathOrURL
	}
	return base + "/" + pathOrURL
}

func NewClient(baseURL string) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: &http.Client{Timeout: defaultTimeout},
	}
}

// CloseIdleConnections closes any idle keep-alive connections on the client's transport.
func (c *Client) CloseIdleConnections() {
	if c != nil && c.httpClient != nil {
		c.httpClient.CloseIdleConnections()
	}
}

// RegisterRequest mirrors nodexa-backend's DeviceRegisterRequest.
type RegisterRequest struct {
	DeviceID            string  `json:"device_id"`
	HardwareFingerprint string  `json:"hardware_fingerprint"`
	IdentityProvider    string  `json:"identity_provider"`
	OSVersion           string  `json:"os_version"`
	AgentVersion        string  `json:"agent_version"`
	Architecture        string  `json:"architecture"`
	Hostname            string  `json:"hostname,omitempty"`
	FleetID             *string `json:"fleet_id,omitempty"`
	DeviceType          *string `json:"device_type,omitempty"`
	// Platform is the host OS (runtime.GOOS: linux, darwin, windows), so
	// the cloud offers this host the right build when updating.
	Platform string `json:"platform,omitempty"`
}

// RegisterResponse mirrors nodexa-backend's DeviceRegisterResponse.
type RegisterResponse struct {
	DeviceID     string `json:"device_id"`
	Token        string `json:"token"`
	RegisteredAt string `json:"registered_at"`
}

// HeartbeatRequest mirrors nodexa-backend's DeviceHeartbeatRequest.
type HeartbeatRequest struct {
	OSVersion    string `json:"os_version,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`

	// Metrics fields mirror internal/health.Report -- see that package for
	// why CPU/Mem/Disk are plain floats (best-effort, 0 on read failure)
	// while TemperatureC is a pointer (nil, not 0, when this device has no
	// readable thermal zone).
	CPUPercent      float64  `json:"cpu_percent"`
	MemUsedPercent  float64  `json:"mem_used_percent"`
	DiskUsedPercent float64  `json:"disk_used_percent"`
	TemperatureC    *float64 `json:"temperature_c,omitempty"`

	MemTotalBytes  uint64 `json:"mem_total_bytes"`
	MemUsedBytes   uint64 `json:"mem_used_bytes"`
	DiskTotalBytes uint64 `json:"disk_total_bytes"`
	DiskUsedBytes  uint64 `json:"disk_used_bytes"`
	// CPUCores is what CPUPercent is a share of.
	CPUCores int `json:"cpu_cores,omitempty"`
	// Load averages over 1/5/15 minutes and seconds since boot.
	LoadAvg1      *float64 `json:"load_avg_1m,omitempty"`
	LoadAvg5      *float64 `json:"load_avg_5m,omitempty"`
	LoadAvg15     *float64 `json:"load_avg_15m,omitempty"`
	UptimeSeconds uint64   `json:"uptime_seconds,omitempty"`

	// Containers is nil on most ticks: nodexa-backend's DeviceHeartbeatRequest
	// treats omitted/null as "unchanged since my last heartbeat that included
	// this" and leaves its stored snapshot alone. The heartbeat loop in
	// cmd/nodexa-agent only populates it when the local container list
	// differs from what it last sent, or 15 minutes have passed since it
	// last did, whichever comes first -- never on every tick, to avoid
	// spamming the backend with an unchanged list. A pointer, not a plain
	// slice: a real "no containers running" report is a non-nil pointer to
	// an empty slice, which Go's own `omitempty` would otherwise conflate
	// with the nil/unchanged case (both have len 0).
	Containers *[]ContainerState `json:"containers,omitempty"`

	// TailscaleIP is this device's tailnet IPv4 address (see internal/vpn's
	// Provider.IP), for the support team's ops-only direct SSH access --
	// nil until connectVPN's goroutine has both a key provisioned and a
	// successful "tailscale ip -4" to report, so early heartbeats omit it
	// like Containers above.
	TailscaleIP *string `json:"tailscale_ip,omitempty"`

	// AgentUpdateStatus reports the live progress of an ongoing agent OTA update
	// so Nodexa Cloud dashboards can show real-time progress.
	AgentUpdateStatus *AgentUpdateProgress `json:"agent_update_status,omitempty"`

	// OSUpdateStatus is AgentUpdateStatus's counterpart for a host OS
	// update (see internal/update.OSUpdater). Omitted unless one is running,
	// so heartbeats stay valid against backends that predate OS updates.
	OSUpdateStatus *AgentUpdateProgress `json:"os_update_status,omitempty"`

	// CloudURL is the backend base URL this heartbeat is sent to, so the
	// backend can tell when a cloud URL change has reached this device.
	CloudURL string `json:"cloud_url,omitempty"`

	// Location is where the device is, if it knows; nil has the backend
	// place it by the public IP this heartbeat comes from.
	Location *Location `json:"location,omitempty"`

	// Apps is each application's resource usage; nil when none could be
	// read (the backend keeps the last report).
	Apps []AppUsage `json:"apps,omitempty"`

	// DeviceType identifies the device runtime class ("native" vs "third_party").
	DeviceType *string `json:"device_type,omitempty"`

	// IsWifi indicates whether Wi-Fi hardware is supported on the device.
	IsWifi *bool `json:"is_wifi,omitempty"`

	// IsGSM indicates whether GSM / cellular hardware is supported on the device.
	IsGSM *bool `json:"is_gsm,omitempty"`

	// Connections lists active connection types (e.g. "ethernet", "wifi", "gsm").
	Connections []string `json:"connections,omitempty"`

	// WifiSSID is the SSID of the currently connected Wi-Fi network (if connected).
	WifiSSID string `json:"wifi_ssid,omitempty"`

	// WifiError reports an error if the device failed to connect to a configured Wi-Fi network.
	WifiError string `json:"wifi_error,omitempty"`

	// WifiNetworks lists available Wi-Fi networks in range.
	WifiNetworks []WifiNetwork `json:"wifi_networks,omitempty"`
}

// AppUsage is one application's (container's) share of the device's
// resources, each a percentage of the whole device: CPU across all cores,
// memory of total RAM, storage of the data disk.
type AppUsage struct {
	Name        string  `json:"name"`
	CPUPercent  float64 `json:"cpu_percent"`
	MemPercent  float64 `json:"mem_percent"`
	MemBytes    uint64  `json:"mem_bytes"`
	DiskPercent float64 `json:"disk_percent"`
	DiskBytes   uint64  `json:"disk_bytes"`
}

// Location is a position in decimal degrees.
type Location struct {
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// AgentUpdateTarget specifies an OTA agent release pinned by Nodexa Cloud.
type AgentUpdateTarget struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256,omitempty"`
	Size    int64  `json:"size,omitempty"`
}

// AgentUpdateProgress represents the live status of an OTA update on the device.
type AgentUpdateProgress struct {
	Version  string `json:"version"`
	State    string `json:"state"`    // "downloading", "verifying", "installing", "completed", "failed"
	Progress int    `json:"progress"` // 0-100 percentage
	Error    string `json:"error,omitempty"`
}

// WifiNetwork represents an available Wi-Fi network detected by scanning.
type WifiNetwork struct {
	SSID          string `json:"ssid"`
	SignalPercent int    `json:"signal_percent"`
	Security      string `json:"security"`
}

// DeviceActionTarget mirrors nodexa-backend's DeviceActionTarget: one
// dashboard-requested action, identified by ID so a repeated heartbeat
// response never runs it twice.
type DeviceActionTarget struct {
	ID     string `json:"id"`
	Action string `json:"action"` // "identify", "restart_services", "reboot", "purge_data", "shutdown", "start_container", "stop_container", "restart_container", "change_wifi", "set_wifi"
	// Container names the target of the *_container actions.
	Container string `json:"container,omitempty"`
	// SSID and Password are used for the change_wifi / set_wifi actions.
	SSID     string `json:"ssid,omitempty"`
	Password string `json:"password,omitempty"`
}

// FleetTransferTarget is a dashboard-requested move to another fleet. The
// agent confirms ID, then stores the fleet the confirmation returns (see
// cmd/nodexa-agent's fleetTransfers).
type FleetTransferTarget struct {
	ID      string `json:"id"`
	FleetID string `json:"fleet_id"`
}

// DeviceActionReport is the agent's progress report for a DeviceActionTarget.
type DeviceActionReport struct {
	ID    string `json:"id"`
	State string `json:"state"` // "running", "completed", "failed"
	Error string `json:"error,omitempty"`
}

// ContainerState mirrors nodexa-backend's app/schemas/device.py::ContainerState
// -- a deliberately narrower wire shape than internal/container.NodexaContainer
// (no Command/StartedAt), since those fields aren't part of what the backend
// models or the dashboard currently needs.
type ContainerState struct {
	Name               string    `json:"name"`
	Image              string    `json:"image"`
	State              string    `json:"state"`
	PID                *int      `json:"pid,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	DeploymentName     string    `json:"deployment_name,omitempty"`
	DeploymentRevision int       `json:"deployment_revision,omitempty"`
}

// HeartbeatResponse mirrors nodexa-backend's DeviceHeartbeatResponse.
type HeartbeatResponse struct {
	DeviceID   string `json:"device_id"`
	Status     string `json:"status"`
	LastSeenAt string `json:"last_seen_at"`
	// DeploymentRevision is the revision of this device's *effective*
	// deployment (nil if it has none). The heartbeat ticker in cmd/
	// nodexa-agent compares this against internal/deploy.Manager's last
	// applied revision to know whether to call GetDeployment.
	DeploymentRevision *int `json:"deployment_revision,omitempty"`

	// AgentUpdate is populated when an admin or user has pinned an agent update
	// for this device in Nodexa Cloud.
	AgentUpdate *AgentUpdateTarget `json:"agent_update,omitempty"`

	// OSUpdate is populated when a host OS version is pinned for this
	// device. Same shape as AgentUpdate: URL points at the OS release's
	// .wic image, whose root partition gets written to the inactive slot.
	OSUpdate *AgentUpdateTarget `json:"os_update,omitempty"`

	// Action is populated while a user-requested device action (identify,
	// reboot, ...) is pending or running for this device.
	Action *DeviceActionTarget `json:"action,omitempty"`

	// FleetTransfer is populated while a fleet transfer awaits this
	// device's confirmation. DeploymentRevision is withheld meanwhile.
	FleetTransfer *FleetTransferTarget `json:"fleet_transfer,omitempty"`

	// CloudURL is populated while the platform's cloud URL differs from
	// the one this device reports: the agent registers through it and, if
	// that works, switches over (see cmd/nodexa-agent's cloudURLChanges).
	CloudURL *string `json:"cloud_url,omitempty"`
}

// DeploymentResponse mirrors nodexa-backend's DeviceDeploymentOut --
// Services/Registries decode directly into internal/container's own
// domain types rather than a separate wire-format mirror, since that
// package is where they're consumed to pull images and generate bundles.
type DeploymentResponse struct {
	DeploymentID string                         `json:"deployment_id"`
	Name         string                         `json:"name"`
	Services     []container.ServiceSpec        `json:"services"`
	Registries   []container.RegistryCredential `json:"registries"`
	Revision     int                            `json:"revision"`
}

// Register calls POST /api/v1/devices/register. This is an idempotent
// upsert on the backend side: safe to call every boot, and always returns
// a fresh token.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	if err := c.do(ctx, http.MethodPost, "/api/v1/devices/register", "", req, &resp); err != nil {
		return nil, fmt.Errorf("backend: register: %w", err)
	}
	return &resp, nil
}

// Heartbeat calls POST /api/v1/devices/{deviceID}/heartbeat, authenticating
// with the token returned by Register.
func (c *Client) Heartbeat(ctx context.Context, deviceID, token string, req HeartbeatRequest) (*HeartbeatResponse, error) {
	var resp HeartbeatResponse
	path := "/api/v1/devices/" + deviceID + "/heartbeat"
	if err := c.do(ctx, http.MethodPost, path, token, req, &resp); err != nil {
		return nil, fmt.Errorf("backend: heartbeat: %w", err)
	}
	return &resp, nil
}

// GetDeployment calls GET /api/v1/devices/{deviceID}/deployment, the
// device-authenticated "effective deployment" endpoint (fleet-level,
// overridden by a device-pinned one -- see nodexa-backend's
// DeploymentService.effective_for_device). A device with no deployment
// assigned is not an error: the backend returns 404, and GetDeployment
// returns (nil, nil) for it, matching Register/Heartbeat's own
// best-effort-from-the-device's-view posture elsewhere in this package.
func (c *Client) GetDeployment(ctx context.Context, deviceID, token string) (*DeploymentResponse, error) {
	path := "/api/v1/devices/" + deviceID + "/deployment"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL()+path, nil)
	if err != nil {
		return nil, fmt.Errorf("backend: get deployment: build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+token)

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("backend: get deployment: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == http.StatusNotFound {
		return nil, nil
	}

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return nil, fmt.Errorf("backend: get deployment: read response: %w", err)
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return nil, fmt.Errorf("backend: get deployment: unexpected status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	var resp DeploymentResponse
	if err := json.Unmarshal(respBytes, &resp); err != nil {
		return nil, fmt.Errorf("backend: get deployment: decode response: %w", err)
	}
	return &resp, nil
}

// ReportUpdateProgress calls POST /api/v1/devices/{deviceID}/update-progress.
// It reports real-time download and installation progress to Nodexa Cloud.
// If the backend returns 404 (endpoint not yet implemented), it is handled
// gracefully without aborting the update.
func (c *Client) ReportUpdateProgress(ctx context.Context, deviceID, token string, progress AgentUpdateProgress) error {
	return c.reportProgress(ctx, "/api/v1/devices/"+deviceID+"/update-progress", token, progress)
}

// ReportOSUpdateProgress calls POST /api/v1/devices/{deviceID}/os-update-progress,
// ReportUpdateProgress's counterpart for host OS updates.
func (c *Client) ReportOSUpdateProgress(ctx context.Context, deviceID, token string, progress AgentUpdateProgress) error {
	return c.reportProgress(ctx, "/api/v1/devices/"+deviceID+"/os-update-progress", token, progress)
}

// ReportActionStatus calls POST /api/v1/devices/{deviceID}/action-status.
func (c *Client) ReportActionStatus(ctx context.Context, deviceID, token string, report DeviceActionReport) error {
	var resp struct {
		Status string `json:"status"`
	}
	return c.do(ctx, http.MethodPost, "/api/v1/devices/"+deviceID+"/action-status", token, report, &resp)
}

// ConfirmFleetTransfer calls POST /api/v1/devices/{deviceID}/fleet-transfer/confirm
// and returns the device's fleet afterwards ("" if none). Safe to repeat.
func (c *Client) ConfirmFleetTransfer(ctx context.Context, deviceID, token, transferID string) (string, error) {
	var resp struct {
		FleetID *string `json:"fleet_id"`
	}
	req := struct {
		ID string `json:"id"`
	}{ID: transferID}
	if err := c.do(ctx, http.MethodPost, "/api/v1/devices/"+deviceID+"/fleet-transfer/confirm", token, req, &resp); err != nil {
		return "", fmt.Errorf("backend: confirm fleet transfer: %w", err)
	}
	if resp.FleetID == nil {
		return "", nil
	}
	return *resp.FleetID, nil
}

func (c *Client) reportProgress(ctx context.Context, path, token string, progress any) error {
	body, err := json.Marshal(progress)
	if err != nil {
		return fmt.Errorf("backend: report progress: encode: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL()+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("backend: report progress: build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("backend: report progress: %w", err)
	}
	defer httpResp.Body.Close()

	if httpResp.StatusCode == http.StatusNotFound {
		return nil
	}
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		respBytes, _ := io.ReadAll(httpResp.Body)
		return fmt.Errorf("backend: report progress: unexpected status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(respBytes)))
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path, token string, reqBody, respBody any) error {
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, method, c.BaseURL()+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return fmt.Errorf("request: %w", err)
	}
	defer httpResp.Body.Close()

	respBytes, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return fmt.Errorf("read response: %w", err)
	}

	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(respBytes)))
	}

	if err := json.Unmarshal(respBytes, respBody); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}
