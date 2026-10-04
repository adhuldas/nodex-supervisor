package commands

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// Health implements `nodexactl health`.
func Health(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	h, err := c.Health(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "Nodexa OS Health")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Overall:\t%s\n", title(h.Overall))
	fmt.Fprintf(out, "Agent:\t%s\n", agentLabel(h.Agent))
	fmt.Fprintf(out, "Runtime:\t%s\n", runtimeLabel(h.Runtime))
	fmt.Fprintf(out, "Network:\t%s\n", networkLabel(h.Network))
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Load Average (1m):\t%.2f\n", h.LoadAvg1)
	fmt.Fprintf(out, "Memory Used:\t%.1f%% (%d / %d bytes)\n", h.MemUsedPercent, h.MemUsedBytes, h.MemTotalBytes)
	fmt.Fprintf(out, "Disk Used:\t%.1f%% (%d / %d bytes)\n", h.DiskUsedPercent, h.DiskUsedBytes, h.DiskTotalBytes)
	fmt.Fprintf(out, "Checked At:\t%s\n", h.Timestamp)
	return out.Flush()
}
