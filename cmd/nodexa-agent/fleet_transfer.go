package main

import (
	"context"
	"log"
	"sync"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/deploy"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/state"
)

// State keys for the fleet a confirmed transfer moved this device to, and
// the flash-time (provisioning) fleet it was stored over.
const (
	stateKeyFleetID            = "fleet_id"
	stateKeyFleetIDProvisioned = "fleet_id_provisioned"
)

// resolveFleetID returns the fleet to register with: the one stored by a
// confirmed fleet transfer, as long as the flash-time config still names
// the fleet it was stored over -- re-provisioning into another fleet wins.
func resolveFleetID(store *state.Store, provisioned *string) *string {
	stored, ok := store.Get(stateKeyFleetID)
	if !ok || stored == "" {
		return provisioned
	}
	if base, _ := store.Get(stateKeyFleetIDProvisioned); base != derefString(provisioned) {
		return provisioned
	}
	return &stored
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// fleetTransfers carries out a fleet transfer handed over in a heartbeat
// response: confirm it with the cloud, then store the fleet the cloud
// answers with, so the next registration (after a reboot or agent restart)
// reports it. Confirming first means a transfer cancelled meanwhile is
// never stored; a crash between the two is covered by nodexa-backend,
// which keeps a device that re-registers with the source fleet in the
// target one. Heartbeats keep carrying the transfer until it's confirmed,
// so a failed attempt (offline, network loss) is simply retried.
type fleetTransfers struct {
	client      *backend.Client
	deviceID    string
	store       *state.Store
	provisioned *string
	deployMgr   *deploy.Manager

	mu       sync.Mutex
	inFlight bool
}

func (f *fleetTransfers) handle(ctx context.Context, token string, target backend.FleetTransferTarget) {
	f.mu.Lock()
	if f.inFlight {
		f.mu.Unlock()
		return
	}
	f.inFlight = true
	f.mu.Unlock()

	go func() {
		defer func() {
			f.mu.Lock()
			f.inFlight = false
			f.mu.Unlock()
		}()

		fleetID, err := f.client.ConfirmFleetTransfer(ctx, f.deviceID, token, target.ID)
		if err != nil {
			log.Printf("warning: fleet transfer %s: %v (will retry)", target.ID, err)
			return
		}
		if err := f.store.Set(stateKeyFleetIDProvisioned, derefString(f.provisioned)); err != nil {
			log.Printf("warning: fleet transfer: storing fleet: %v", err)
		} else if err := f.store.Set(stateKeyFleetID, fleetID); err != nil {
			log.Printf("warning: fleet transfer: storing fleet: %v", err)
		}
		// The new fleet's revision numbers are unrelated to the old one's:
		// apply whatever the next heartbeat names, even the same number.
		f.deployMgr.ForgetApplied()
		log.Printf("fleet transfer %s confirmed: now in fleet %q", target.ID, fleetID)
	}()
}
