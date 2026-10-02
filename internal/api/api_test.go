package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/update"
)

type fakeRunner struct {
	state   *container.NodexaContainer
	execOut string
	execErr string
	execRet int
}

func TestContainerExecAPI(t *testing.T) {
	bus := events.NewBus(10)
	dir := t.TempDir()
	netDir := t.TempDir()

	netMgr, err := container.NewNetworkManager(netDir, bus)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}

	containerMgr := container.NewManager(dir, "", t.TempDir(), t.TempDir(), "runc", t.TempDir(), bus)
	containerMgr.SetNetworkManager(netMgr)

	server := New("/tmp/test-agent.sock", Deps{
		Containers: containerMgr,
		Networks:   netMgr,
		Events:     bus,
	})

	// When container does not exist, exec should return an error
	reqBody := `{"command":["echo","hello"]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/containers/nonexistent/exec", bytes.NewBufferString(reqBody))
	w := httptest.NewRecorder()

	server.http.Handler.ServeHTTP(w, req)

	if w.Code == http.StatusOK {
		t.Errorf("expected error status for nonexistent container, got %d", w.Code)
	}

	var errResp map[string]string
	_ = json.NewDecoder(w.Body).Decode(&errResp)
	if errResp["error"] == "" {
		t.Errorf("expected error message in response body")
	}
}

func TestContainerInspectAPI(t *testing.T) {
	bus := events.NewBus(10)
	dir := t.TempDir()
	netDir := t.TempDir()

	netMgr, err := container.NewNetworkManager(netDir, bus)
	if err != nil {
		t.Fatalf("NewNetworkManager: %v", err)
	}

	containerMgr := container.NewManager(dir, "", t.TempDir(), t.TempDir(), "runc", t.TempDir(), bus)
	containerMgr.SetNetworkManager(netMgr)

	server := New("/tmp/test-agent.sock", Deps{
		Containers: containerMgr,
		Networks:   netMgr,
		Events:     bus,
	})

	// 1. Non-existent container returns 404
	req := httptest.NewRequest(http.MethodGet, "/v1/containers/missing-container", nil)
	w := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("expected 404 for missing container, got %d", w.Code)
	}
}

func TestUpdateAPI(t *testing.T) {
	bus := events.NewBus(10)
	tmpDir := t.TempDir()
	updater := update.NewAgentUpdater(tmpDir+"/base", tmpDir+"/ota", tmpDir, bus)

	server := New("/tmp/test-agent.sock", Deps{
		Update: updater,
		Events: bus,
	})

	// Test GET /v1/update/status
	req := httptest.NewRequest(http.MethodGet, "/v1/update/status", nil)
	w := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var status update.AgentStatus
	if err := json.NewDecoder(w.Body).Decode(&status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.OTAPresent {
		t.Fatal("expected OTAPresent=false initially")
	}

	// Test POST /v1/update/rollback
	reqRollback := httptest.NewRequest(http.MethodPost, "/v1/update/rollback", nil)
	wRollback := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(wRollback, reqRollback)
	if wRollback.Code != http.StatusOK {
		t.Fatalf("expected 200 on rollback, got %d", wRollback.Code)
	}
}


