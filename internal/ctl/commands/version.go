package commands

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// Version implements `nodexactl version`.
func Version(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	v, err := c.Version(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Nodexa OS\t%s\n", v.OSVersion)
	fmt.Fprintf(out, "Nodexa Agent\t%s\n", v.AgentVersion)
	fmt.Fprintf(out, "Build Commit\t%s\n", v.Commit)
	fmt.Fprintf(out, "Build Date\t%s\n", v.BuildDate)
	fmt.Fprintf(out, "Architecture\t%s\n", v.Architecture)
	return out.Flush()
}
