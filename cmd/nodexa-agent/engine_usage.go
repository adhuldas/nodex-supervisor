package main

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
)

// engineUsageInterval is how often volumes, images and logs are re-measured.
// `docker system df -v` can take a while on a busy host, so it runs here in
// the background and the heartbeat only picks up the last finished result.
const engineUsageInterval = 5 * time.Minute

// engineUsageTimeout abandons a measurement that hangs on a stuck daemon.
const engineUsageTimeout = 2 * time.Minute

type engineUsageCollector struct {
	containers *container.NodexaContainerManager

	mu     sync.Mutex
	latest *container.EngineUsage
	kick   chan struct{}
}

func newEngineUsageCollector(m *container.NodexaContainerManager) *engineUsageCollector {
	return &engineUsageCollector{containers: m, kick: make(chan struct{}, 1)}
}

// Run measures until ctx is cancelled. Call it in its own goroutine.
func (c *engineUsageCollector) Run(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-c.kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		c.measure(ctx)
		timer.Reset(engineUsageInterval)
	}
}

func (c *engineUsageCollector) measure(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, engineUsageTimeout)
	defer cancel()
	u, err := c.containers.Usage(ctx)
	if err != nil {
		log.Printf("warning: engine usage: %v", err)
		return
	}
	c.mu.Lock()
	c.latest = u
	c.mu.Unlock()
}

// Latest returns the last finished measurement, or nil if there is none yet.
func (c *engineUsageCollector) Latest() *container.EngineUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.latest
}

// Refresh asks for a new measurement now, e.g. after space was reclaimed.
func (c *engineUsageCollector) Refresh() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}
