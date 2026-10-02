package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/identity"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/state"
)

func TestResolveCloudURLPrefersChangedURL(t *testing.T) {
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	urlA, urlC := "https://a.example.com", "https://c.example.com"

	if got := resolveCloudURL(store, nil); got != nil {
		t.Fatalf("standalone: got %q", *got)
	}
	if got := resolveCloudURL(store, &urlA); got != &urlA {
		t.Fatalf("no change stored: got %v", derefString(got))
	}

	_ = store.Set(stateKeyCloudURLProvisioned, urlA)
	_ = store.Set(stateKeyCloudURL, "https://b.example.com")
	if got := derefString(resolveCloudURL(store, &urlA)); got != "https://b.example.com" {
		t.Fatalf("after change: got %q", got)
	}
	// A stored URL never makes a standalone device talk to a backend.
	if got := resolveCloudURL(store, nil); got != nil {
		t.Fatalf("standalone after change: got %q", *got)
	}
	// Re-provisioned with another URL: the flash-time config wins.
	if got := derefString(resolveCloudURL(store, &urlC)); got != urlC {
		t.Fatalf("after re-provisioning: got %q", got)
	}
}

func newCloudURLChanges(t *testing.T, oldURL string) (*cloudURLChanges, *state.Store) {
	t.Helper()
	store, err := state.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &cloudURLChanges{
		client:      backend.NewClient(oldURL),
		identity:    &identity.Identity{DeviceID: "dev-1"},
		store:       store,
		provisioned: &oldURL,
		fleetID:     func() *string { return nil },
		token:       &tokenHolder{token: "old-token"},
		bus:         events.NewBus(16),
	}
	return c, store
}

func waitIdle(t *testing.T, c *cloudURLChanges) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		c.mu.Lock()
		busy := c.inFlight
		c.mu.Unlock()
		if !busy {
			return
		}
	}
	t.Fatal("cloud URL change still in flight")
}

func TestCloudURLChangeSwitchesAfterRegistering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/devices/register" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"device_id":"dev-1","token":"new-token","registered_at":"2026-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	c, store := newCloudURLChanges(t, "https://old.example.com")
	c.handle(context.Background(), srv.URL+"/")
	waitIdle(t, c)

	if got := c.client.BaseURL(); got != srv.URL {
		t.Fatalf("client base URL = %q, want %q", got, srv.URL)
	}
	if got := c.token.Get(); got != "new-token" {
		t.Fatalf("token = %q, want new-token", got)
	}
	if got, _ := store.Get(stateKeyCloudURL); got != srv.URL {
		t.Fatalf("stored URL = %q", got)
	}
	if got, _ := store.Get(stateKeyCloudURLProvisioned); got != "https://old.example.com" {
		t.Fatalf("stored provisioned URL = %q", got)
	}
}

func TestCloudURLChangeKeepsOldURLWhenNewOneFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()

	c, store := newCloudURLChanges(t, "https://old.example.com")
	c.handle(context.Background(), srv.URL)
	waitIdle(t, c)

	if got := c.client.BaseURL(); got != "https://old.example.com" {
		t.Fatalf("client base URL = %q, want the old one", got)
	}
	if got := c.token.Get(); got != "old-token" {
		t.Fatalf("token = %q, want old-token", got)
	}
	if _, ok := store.Get(stateKeyCloudURL); ok {
		t.Fatal("URL stored despite failed registration")
	}
}
