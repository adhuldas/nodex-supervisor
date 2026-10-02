package main

import (
	"context"
	"log"
	"strings"
	"sync"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/identity"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/state"
)

// State keys for the cloud URL a cloud URL change moved this device to, and
// the flash-time (provisioning) cloud URL it was stored over.
const (
	stateKeyCloudURL            = "cloud_url"
	stateKeyCloudURLProvisioned = "cloud_url_provisioned"
)

// resolveCloudURL returns the backend to talk to: the one stored by a cloud
// URL change, as long as the flash-time config still names the URL it was
// stored over -- re-provisioning with another URL wins, same as
// resolveFleetID. nil means standalone.
func resolveCloudURL(store *state.Store, provisioned *string) *string {
	if provisioned == nil {
		return nil
	}
	stored, ok := store.Get(stateKeyCloudURL)
	if !ok || stored == "" {
		return provisioned
	}
	if base, _ := store.Get(stateKeyCloudURLProvisioned); base != *provisioned {
		return provisioned
	}
	return &stored
}

// cloudURLChanges carries out a cloud URL change handed over in a
// heartbeat response: register through the new URL first, and only once
// that works store it and switch the shared client (and token) over. A
// URL that doesn't work yet (DNS not live, typo) leaves the device on the
// one it has; heartbeats keep carrying the change, so it's simply retried.
// The backend counts the device as moved once its heartbeats report the
// new URL (backend.HeartbeatRequest.CloudURL).
type cloudURLChanges struct {
	client      *backend.Client
	identity    *identity.Identity
	store       *state.Store
	provisioned *string
	fleetID     func() *string
	token       *tokenHolder
	bus         *events.Bus

	mu       sync.Mutex
	inFlight bool
}

func (c *cloudURLChanges) handle(ctx context.Context, target string) {
	target = strings.TrimRight(target, "/")
	if target == "" || target == c.client.BaseURL() {
		return
	}
	c.mu.Lock()
	if c.inFlight {
		c.mu.Unlock()
		return
	}
	c.inFlight = true
	c.mu.Unlock()

	go func() {
		defer func() {
			c.mu.Lock()
			c.inFlight = false
			c.mu.Unlock()
		}()

		resp, err := backend.NewClient(target).Register(ctx, registerRequest(c.identity, c.fleetID()))
		if err != nil {
			log.Printf("warning: cloud URL change to %s: %v (will retry)", target, err)
			return
		}
		if err := c.store.Set(stateKeyCloudURLProvisioned, derefString(c.provisioned)); err != nil {
			log.Printf("warning: cloud URL change: storing URL: %v", err)
		} else if err := c.store.Set(stateKeyCloudURL, target); err != nil {
			log.Printf("warning: cloud URL change: storing URL: %v", err)
		}
		previous := c.client.BaseURL()
		c.client.SetBaseURL(target)
		c.token.Set(resp.Token)
		c.bus.Emit(events.DeviceRegistered, "registered with nodexa-backend", events.Fieldsf("cloud_url", "%s", target))
		log.Printf("cloud URL changed: %s -> %s", previous, target)
	}()
}
