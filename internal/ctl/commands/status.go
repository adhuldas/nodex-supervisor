package commands

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// Status implements `nodexactl status`.
func Status(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	s, err := c.Status(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "Nodexa OS Status")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Device ID:\t%s\n", s.DeviceID)
	fmt.Fprintf(out, "OS Version:\t%s\n", s.OSVersion)
	fmt.Fprintf(out, "Agent Version:\t%s\n", s.AgentVersion)
	fmt.Fprintf(out, "Architecture:\t%s\n", s.Architecture)
	fmt.Fprintf(out, "Uptime:\t%s\n", s.UptimeHuman)
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Agent:\t%s\n", agentLabel(s.Agent))
	fmt.Fprintf(out, "Runtime:\t%s\n", runtimeLabel(s.Runtime))
	fmt.Fprintf(out, "Network:\t%s\n", networkLabel(s.Network))
	return out.Flush()
}
