package commands

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// UpdateStatus implements `nodexactl update status`.
func UpdateStatus(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	s, err := c.UpdateStatus(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "Nodexa Agent OTA Status")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Current Version:\t%s\n", s.CurrentVersion)
	fmt.Fprintf(out, "Running Binary:\t%s\n", s.RunningBinary)

	if s.IsOTAActive {
		fmt.Fprintf(out, "OTA Active:\tYes (Running OTA binary)\n")
	} else {
		fmt.Fprintf(out, "OTA Active:\tNo (Running base golden image)\n")
	}

	if s.OTAPresent {
		fmt.Fprintf(out, "OTA Binary Present:\tYes\n")
	} else {
		fmt.Fprintf(out, "OTA Binary Present:\tNo\n")
	}

	if s.RollbackAvailable {
		fmt.Fprintf(out, "Rollback Available:\tYes (run: nodexa update rollback)\n")
	} else {
		fmt.Fprintf(out, "Rollback Available:\tNo\n")
	}

	return out.Flush()
}

// UpdateRollback implements `nodexactl update rollback`.
func UpdateRollback(ctx context.Context, c *client.Client) error {
	if err := c.UpdateRollback(ctx); err != nil {
		return err
	}
	fmt.Println("Rolled back to base golden binary. Agent supervisor will restart into base version.")
	return nil
}

// UpdateApply implements `nodexactl update apply <url|file>`.
func UpdateApply(ctx context.Context, c *client.Client, target, expectedSHA, version string) error {
	req := client.UpdateApplyRequest{
		SHA256:  expectedSHA,
		Version: version,
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		req.URL = target
		fmt.Printf("Submitting OTA update from URL: %s...\n", target)
	} else {
		req.Path = target
		fmt.Printf("Applying local update binary: %s...\n", target)
	}

	if err := c.UpdateApply(ctx, req); err != nil {
		return err
	}

	fmt.Println("Update request accepted. Use 'nodexa update status' to view progress.")
	return nil
}
