package backend

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/devices/register" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		var req RegisterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.DeviceID != "ndx_dev_test" {
			t.Fatalf("unexpected device_id: %s", req.DeviceID)
		}
		if req.FleetID == nil || *req.FleetID != "fleet-a" {
			t.Fatalf("unexpected fleet_id: %+v", req.FleetID)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			DeviceID:     req.DeviceID,
			Token:        "test-token",
			RegisteredAt: "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	fleetID := "fleet-a"
	client := NewClient(srv.URL)
	resp, err := client.Register(context.Background(), RegisterRequest{
		DeviceID:            "ndx_dev_test",
		HardwareFingerprint: "fp",
		IdentityProvider:    "qemu",
		OSVersion:           "0.1.5",
		AgentVersion:        "0.1.5",
		Architecture:        "arm64",
		FleetID:             &fleetID,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if resp.Token != "test-token" {
		t.Fatalf("unexpected token: %s", resp.Token)
	}
}

func TestRegisterThirdPartyDevicePayload(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// nodexa-backend's DeviceRegisterRequest is extra="forbid": any
		// other key fails every registration with a 422.
		var raw map[string]any
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		accepted := map[string]bool{
			"device_id": true, "hardware_fingerprint": true, "identity_provider": true,
			"os_version": true, "agent_version": true, "architecture": true,
			"hostname": true, "fleet_id": true, "device_type": true, "platform": true,
		}
		for key := range raw {
			if !accepted[key] {
				t.Errorf("register payload has %q, which nodexa-backend rejects", key)
			}
		}
		var req RegisterRequest
		_ = json.Unmarshal(body, &req)
		if req.DeviceType == nil || *req.DeviceType != "third_party" {
			t.Fatalf("expected device_type 'third_party', got %+v", req.DeviceType)
		}
		if req.IdentityProvider != "third_party" {
			t.Fatalf("expected identity_provider 'third_party', got %s", req.IdentityProvider)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(RegisterResponse{
			DeviceID:     req.DeviceID,
			Token:        "test-token",
			RegisteredAt: "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	deviceType := "third_party"
	client := NewClient(srv.URL)
	_, err := client.Register(context.Background(), RegisterRequest{
		DeviceID:            "ndx_dev_third_party",
		HardwareFingerprint: "fp",
		IdentityProvider:    "third_party",
		OSVersion:           "1.0.0",
		AgentVersion:        "0.3.7",
		Architecture:        "arm64",
		DeviceType:          &deviceType,
	})
	if err != nil {
		t.Fatalf("Register third-party device: %v", err)
	}
}

func TestHeartbeatSendsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if !strings.HasSuffix(r.URL.Path, "/heartbeat") {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{
			DeviceID:   "ndx_dev_test",
			Status:     "online",
			LastSeenAt: "2026-01-01T00:00:00Z",
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	resp, err := client.Heartbeat(context.Background(), "ndx_dev_test", "secret-token", HeartbeatRequest{
		AgentVersion: "0.2.0",
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if gotAuth != "Bearer secret-token" {
		t.Fatalf("unexpected Authorization header: %q", gotAuth)
	}
	if resp.Status != "online" {
		t.Fatalf("unexpected status: %s", resp.Status)
	}
}

func TestHeartbeatSendsMetrics(t *testing.T) {
	var got HeartbeatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{DeviceID: "ndx_dev_test", Status: "online"})
	}))
	defer srv.Close()

	temp := 47.5
	deviceType := "third_party"
	client := NewClient(srv.URL)
	_, err := client.Heartbeat(context.Background(), "ndx_dev_test", "secret-token", HeartbeatRequest{
		AgentVersion:    "0.2.0",
		CPUPercent:      12.5,
		MemUsedPercent:  60,
		DiskUsedPercent: 71,
		TemperatureC:    &temp,
		DeviceType:      &deviceType,
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got.CPUPercent != 12.5 || got.MemUsedPercent != 60 || got.DiskUsedPercent != 71 {
		t.Fatalf("unexpected metrics: %+v", got)
	}
	if got.TemperatureC == nil || *got.TemperatureC != 47.5 {
		t.Fatalf("unexpected temperature: %+v", got.TemperatureC)
	}
	if got.DeviceType == nil || *got.DeviceType != "third_party" {
		t.Fatalf("unexpected device_type: %+v", got.DeviceType)
	}
}

func TestHeartbeatOmitsNilTemperature(t *testing.T) {
	var gotRaw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotRaw); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{DeviceID: "ndx_dev_test", Status: "online"})
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	_, err := client.Heartbeat(context.Background(), "ndx_dev_test", "secret-token", HeartbeatRequest{
		AgentVersion: "0.2.0",
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if _, present := gotRaw["temperature_c"]; present {
		t.Fatalf("expected temperature_c to be omitted, got %+v", gotRaw)
	}
}

func TestRegisterNon2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"detail": "invalid payload"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	_, err := client.Register(context.Background(), RegisterRequest{DeviceID: "ndx_dev_test"})
	if err == nil {
		t.Fatal("expected error for non-2xx response")
	}
}

func TestRegisterMalformedResponseIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	_, err := client.Register(context.Background(), RegisterRequest{DeviceID: "ndx_dev_test"})
	if err == nil {
		t.Fatal("expected error for malformed response body")
	}
}

func TestHeartbeatWithAgentUpdateAndStatus(t *testing.T) {
	var gotReq HeartbeatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(HeartbeatResponse{
			DeviceID: "ndx_dev_test",
			Status:   "online",
			AgentUpdate: &AgentUpdateTarget{
				Version: "0.2.0",
				URL:     "https://cloud.nodexa.io/updates/nodexa-agent-0.2.0-linux-armv7.tar.gz",
				SHA256:  "abcdef123456",
			},
		})
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	resp, err := client.Heartbeat(context.Background(), "ndx_dev_test", "secret-token", HeartbeatRequest{
		AgentVersion: "0.1.5",
		AgentUpdateStatus: &AgentUpdateProgress{
			Version:  "0.2.0",
			State:    "downloading",
			Progress: 45,
		},
	})
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	if gotReq.AgentUpdateStatus == nil || gotReq.AgentUpdateStatus.Progress != 45 {
		t.Fatalf("expected AgentUpdateStatus progress 45, got %+v", gotReq.AgentUpdateStatus)
	}

	if resp.AgentUpdate == nil || resp.AgentUpdate.Version != "0.2.0" {
		t.Fatalf("expected AgentUpdate version 0.2.0, got %+v", resp.AgentUpdate)
	}
}

func TestReportUpdateProgress(t *testing.T) {
	var gotProgress AgentUpdateProgress
	var gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/devices/ndx_dev_test/update-progress" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		gotToken = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotProgress); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status": "ok"}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	err := client.ReportUpdateProgress(context.Background(), "ndx_dev_test", "test-bearer", AgentUpdateProgress{
		Version:  "0.2.0",
		State:    "installing",
		Progress: 90,
	})
	if err != nil {
		t.Fatalf("ReportUpdateProgress: %v", err)
	}
	if gotToken != "Bearer test-bearer" {
		t.Fatalf("expected Bearer test-bearer, got %q", gotToken)
	}
	if gotProgress.State != "installing" || gotProgress.Progress != 90 {
		t.Fatalf("unexpected progress: %+v", gotProgress)
	}
}

func TestReportUpdateProgress404Ignored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	client := NewClient(srv.URL)
	err := client.ReportUpdateProgress(context.Background(), "ndx_dev_test", "test-bearer", AgentUpdateProgress{
		Version:  "0.2.0",
		State:    "downloading",
		Progress: 10,
	})
	if err != nil {
		t.Fatalf("expected 404 to be non-fatal, got: %v", err)
	}
}
