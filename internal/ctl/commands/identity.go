package commands

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// Identity implements `nodexactl identity`.
func Identity(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	id, err := c.Identity(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "Nodexa Device Identity")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Device ID:\t%s\n", id.DeviceID)
	fmt.Fprintf(out, "Hardware Fingerprint:\t%s\n", id.Fingerprint)
	fmt.Fprintf(out, "Identity Provider:\t%s\n", id.Provider)
	fmt.Fprintf(out, "Sources:\t%s\n", strings.Join(id.SourceKeys, ", "))
	fmt.Fprintf(out, "Created:\t%s\n", id.CreatedAt)
	return out.Flush()
}
