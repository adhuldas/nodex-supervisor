package commands

import (
	"context"
	"fmt"
	"text/tabwriter"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// Images implements `nodexactl images`.
func Images(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	list, err := c.Images(ctx)
	if err != nil {
		return err
	}

	if len(list) == 0 {
		fmt.Fprintln(out, "No images found.")
		return out.Flush()
	}

	fmt.Fprintln(out, "NAME\tSIZE\tPULLED")
	for _, img := range list {
		fmt.Fprintf(out, "%s\t%s\t%s\n", img.Name, formatSize(img.SizeBytes), img.PulledAt.Format("2006-01-02T15:04:05Z07:00"))
	}
	return out.Flush()
}

// ImageRemove implements `nodexactl images rm <name>`.
func ImageRemove(ctx context.Context, c *client.Client, name string) error {
	if err := c.ImageRemove(ctx, name); err != nil {
		return err
	}
	fmt.Printf("Image %q removed.\n", name)
	return nil
}

// formatSize renders bytes the way docker/nerdctl's "SIZE" column does:
// the smallest binary unit that keeps the number under 1024, one decimal
// place.
func formatSize(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%dB", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}
