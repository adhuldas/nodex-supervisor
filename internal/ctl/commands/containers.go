package commands

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// Containers implements `nodexactl containers`.
func Containers(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	list, err := c.Containers(ctx)
	if err != nil {
		return err
	}

	if len(list) == 0 {
		fmt.Fprintln(out, "No containers found.")
		return out.Flush()
	}

	fmt.Fprintln(out, "NAME\tIMAGE\tSTATE\tPID\tCREATED")
	for _, ctr := range list {
		pid := ""
		if ctr.PID != 0 {
			pid = fmt.Sprintf("%d", ctr.PID)
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", ctr.Name, ctr.Image, title(ctr.State), pid, ctr.CreatedAt)
	}
	return out.Flush()
}

// ContainerStats implements `nodexactl containers stats <name>`.
func ContainerStats(ctx context.Context, c *client.Client, out *tabwriter.Writer, name string) error {
	s, err := c.ContainerStats(ctx, name)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Container:\t%s\n", s.Name)
	fmt.Fprintf(out, "CPU Time:\t%.2fs\n", s.CPUUsageSeconds)
	fmt.Fprintf(out, "Memory Usage:\t%d bytes\n", s.MemoryUsageBytes)
	if s.MemoryLimitBytes > 0 {
		fmt.Fprintf(out, "Memory Limit:\t%d bytes\n", s.MemoryLimitBytes)
	}
	fmt.Fprintf(out, "PIDs:\t%d\n", s.PIDs)
	return out.Flush()
}

// ContainerLogs implements `nodexactl logs <name>`.
func ContainerLogs(ctx context.Context, c *client.Client, name string, tail int) error {
	l, err := c.ContainerLogs(ctx, name, tail)
	if err != nil {
		return err
	}
	fmt.Print(l.Log)
	return nil
}

// ContainerLogsFollow implements `nodexactl logs -f <name>`: prints the
// existing log, then streams newly written output until ctx is canceled
// (see main.go's SIGINT/SIGTERM handling around this call).
func ContainerLogsFollow(ctx context.Context, c *client.Client, name string, tail int) error {
	return c.ContainerLogsFollow(ctx, name, tail, os.Stdout)
}

// ContainerStart implements `nodexactl containers start <name>`.
func ContainerStart(ctx context.Context, c *client.Client, name string) error {
	if err := c.ContainerStart(ctx, name); err != nil {
		return err
	}
	fmt.Printf("Container %q started.\n", name)
	return nil
}

// ContainerStop implements `nodexactl containers stop <name>`.
func ContainerStop(ctx context.Context, c *client.Client, name string) error {
	if err := c.ContainerStop(ctx, name); err != nil {
		return err
	}
	fmt.Printf("Container %q stopped.\n", name)
	return nil
}

// ContainerRemove implements `nodexactl containers rm <name>`.
func ContainerRemove(ctx context.Context, c *client.Client, name string) error {
	if err := c.ContainerRemove(ctx, name); err != nil {
		return err
	}
	fmt.Printf("Container %q removed.\n", name)
	return nil
}

// ContainerInspect implements `nodexactl containers inspect <name>`.
func ContainerInspect(ctx context.Context, c *client.Client, out *tabwriter.Writer, name string) error {
	ctr, err := c.ContainerInspect(ctx, name)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Name:\t%s\n", ctr.Name)
	fmt.Fprintf(out, "Image:\t%s\n", ctr.Image)
	fmt.Fprintf(out, "State:\t%s\n", title(ctr.State))
	if ctr.PID != 0 {
		fmt.Fprintf(out, "PID:\t%d\n", ctr.PID)
	}
	if len(ctr.Command) > 0 {
		fmt.Fprintf(out, "Command:\t%s\n", strings.Join(ctr.Command, " "))
	}
	if ctr.Network != "" {
		fmt.Fprintf(out, "Network:\t%s\n", ctr.Network)
	}
	if ctr.IPAddress != "" {
		fmt.Fprintf(out, "IP Address:\t%s\n", ctr.IPAddress)
	}
	if ctr.DeploymentName != "" {
		fmt.Fprintf(out, "Deployment:\t%s (rev %d)\n", ctr.DeploymentName, ctr.DeploymentRevision)
	}
	fmt.Fprintf(out, "Created:\t%s\n", ctr.CreatedAt)
	if ctr.StartedAt != "" {
		fmt.Fprintf(out, "Started:\t%s\n", ctr.StartedAt)
	}
	return out.Flush()
}
