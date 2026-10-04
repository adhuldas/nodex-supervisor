//go:build !linux && !darwin

package storage

import "context"

// collect: the directory layout this breakdown reads is Linux's. Other
// platforms report none, and the dashboard shows it as not reported.
func collect(ctx context.Context, diskPath, dataDir string) (*Breakdown, error) {
	return nil, nil
}
