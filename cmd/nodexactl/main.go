// Command nodexactl is Nodexa OS's command-line management tool. It never
// touches system state directly: every command is a thin call over
// nodexa-agent's local control API at /run/nodexa/agent.sock.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/commands"
)

func newWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: nodexactl <command> [arguments]

Commands:
  status                     Show overall device status
  version                    Show Nodexa OS and Agent version information
  identity                   Show stable device identity
  health                     Show detailed system health
  network                    Show network interfaces and Tailscale VPN status
  network ps                 List container networks (alias: network ls)
  network create <name>      Create a container network
  network rm <name>          Remove a container network
  network inspect <name>     Show detailed network information
  network wifi [list|scan]   List available Wi-Fi networks
  network wifi connect <ssid> Connect to / change Wi-Fi network
  wifi [list|scan]           List available Wi-Fi networks (alias for network wifi)
  wifi connect <ssid>        Connect to / change Wi-Fi network
  containers                 List locally managed containers
  containers stats <name>    Show resource usage for a container
  containers inspect <name>  Show detailed container information
  containers start <name>    Start a container
  containers stop <name>     Stop a container
  containers rm <name>       Remove a container completely (bundle, logs, volumes -- no trace left)
  images                     List images cached locally (pulled by deployments)
  images rm <name>           Remove a cached image (does not affect running/deployed containers)
  logs [--tail N] [-f] <name>  Show a container's captured stdout/stderr
                              (-f/--follow streams new output; Ctrl+C to stop)
  exec [-it] <name> [cmd...]   Run a command inside a running container
  inspect <name>             Show detailed container (or network) information
  update [status]            Show agent OTA update status
  update apply <url|file>    Apply an agent update from remote URL or local file
  update rollback            Revert agent to base golden image
  ps                         Alias for "containers" (docker/nerdctl muscle memory)
  stats <name>               Alias for "containers stats" (docker/nerdctl muscle memory)
  rm <name>                  Alias for "containers rm" (or "rm network <name>")
  rmi <name>                 Alias for "images rm" (docker/nerdctl muscle memory)
  create network <name>      Alias for "network create" (docker/nerdctl muscle memory)

Environment:
  NODEXA_AGENT_SOCKET   Override the agent socket path (default: /run/nodexa/agent.sock, /var/run/nodexa/agent.sock on macOS)`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	socketPath := client.DefaultSocketPath
	if v := os.Getenv("NODEXA_AGENT_SOCKET"); v != "" {
		socketPath = v
	}
	c := client.New(socketPath)

	// signalCtx has no fixed deadline -- only Ctrl+C (or SIGTERM) cancels
	// it -- because `logs -f` is meant to stream indefinitely. Every other
	// command instead uses ctx below, a 10s-bounded child of it, since
	// those are all single-request/response calls that should never hang.
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithTimeout(signalCtx, 10*time.Second)
	defer cancel()

	var err error
	switch os.Args[1] {
	case "status":
		err = commands.Status(ctx, c, newWriter())
	case "version":
		err = commands.Version(ctx, c, newWriter())
	case "identity":
		err = commands.Identity(ctx, c, newWriter())
	case "health":
		err = commands.Health(ctx, c, newWriter())
	case "network":
		err = dispatchNetwork(ctx, c, os.Args[2:])
	case "wifi":
		err = dispatchWifi(ctx, c, os.Args[2:])
	case "create":
		err = dispatchCreate(ctx, c, os.Args[2:])
	case "containers", "ps":
		err = dispatchContainers(ctx, c, os.Args[2:])
	case "images":
		err = dispatchImages(ctx, c, os.Args[2:])
	case "rmi":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: nodexactl rmi <name>")
		} else {
			err = commands.ImageRemove(ctx, c, os.Args[2])
		}
	case "stats":
		if len(os.Args) < 3 {
			err = fmt.Errorf("usage: nodexactl stats <name>")
		} else {
			err = commands.ContainerStats(ctx, c, newWriter(), os.Args[2])
		}
	case "rm":
		err = dispatchRm(ctx, c, os.Args[2:])
	case "logs":
		err = dispatchLogs(ctx, signalCtx, c, os.Args[2:])
	case "exec":
		err = dispatchExec(signalCtx, c, os.Args[2:])
	case "inspect":
		err = dispatchUniversalInspect(ctx, c, os.Args[2:])
	case "update":
		err = dispatchUpdate(ctx, c, os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "nodexactl: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "nodexactl: %v\n", err)
		os.Exit(1)
	}
}

func dispatchContainers(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		return commands.Containers(ctx, c, newWriter())
	}

	switch args[0] {
	case "exec":
		return dispatchExec(ctx, c, args[1:])
	case "inspect":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl containers inspect <name>")
		}
		return commands.ContainerInspect(ctx, c, newWriter(), args[1])
	case "stats":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl containers stats <name>")
		}
		return commands.ContainerStats(ctx, c, newWriter(), args[1])
	case "start":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl containers start <name>")
		}
		return commands.ContainerStart(ctx, c, args[1])
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl containers stop <name>")
		}
		return commands.ContainerStop(ctx, c, args[1])
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl containers rm <name>")
		}
		return commands.ContainerRemove(ctx, c, args[1])
	default:
		return fmt.Errorf("unknown containers subcommand %q", args[0])
	}
}

func dispatchImages(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		return commands.Images(ctx, c, newWriter())
	}

	switch args[0] {
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl images rm <name>")
		}
		return commands.ImageRemove(ctx, c, args[1])
	default:
		return fmt.Errorf("unknown images subcommand %q", args[0])
	}
}

// dispatchLogs parses `nodexactl logs` arguments and runs the command.
// ctx is used for the ordinary (non-follow) request; followCtx -- unbounded
// except for Ctrl+C/SIGTERM, see main() -- is used instead when -f/--follow
// is given, since that request is meant to run indefinitely.
func dispatchLogs(ctx, followCtx context.Context, c *client.Client, args []string) error {
	const usageMsg = "usage: nodexactl logs [--tail N] [-f|--follow] <name>"

	tail := 0
	follow := false
	var name string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--tail":
			if i+1 >= len(args) {
				return fmt.Errorf(usageMsg)
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil || n < 0 {
				return fmt.Errorf("invalid --tail value %q", args[i+1])
			}
			tail = n
			i++
		case "-f", "--follow":
			follow = true
		default:
			if name != "" {
				return fmt.Errorf(usageMsg)
			}
			name = args[i]
		}
	}

	if name == "" {
		return fmt.Errorf(usageMsg)
	}
	if follow {
		return commands.ContainerLogsFollow(followCtx, c, name, tail)
	}
	return commands.ContainerLogs(ctx, c, name, tail)
}

func parseNetworkCreateArgs(args []string) (name, driver, subnet, gateway string, err error) {
	driver = "bridge"
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-d", "--driver":
			if i+1 >= len(args) {
				return "", "", "", "", fmt.Errorf("flag requires an argument: %s", args[i])
			}
			driver = args[i+1]
			i++
		case "--subnet":
			if i+1 >= len(args) {
				return "", "", "", "", fmt.Errorf("flag requires an argument: --subnet")
			}
			subnet = args[i+1]
			i++
		case "--gateway":
			if i+1 >= len(args) {
				return "", "", "", "", fmt.Errorf("flag requires an argument: --gateway")
			}
			gateway = args[i+1]
			i++
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", "", "", fmt.Errorf("unknown flag: %s", args[i])
			}
			if name != "" {
				return "", "", "", "", fmt.Errorf("unexpected argument: %s", args[i])
			}
			name = args[i]
		}
	}
	if name == "" {
		return "", "", "", "", fmt.Errorf("network name is required")
	}
	return name, driver, subnet, gateway, nil
}

func dispatchNetwork(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		return commands.Network(ctx, c, newWriter())
	}
	switch args[0] {
	case "ps", "ls", "list":
		return commands.NetworksList(ctx, c, newWriter())
	case "create":
		name, driver, subnet, gateway, err := parseNetworkCreateArgs(args[1:])
		if err != nil {
			return err
		}
		return commands.NetworkCreate(ctx, c, name, driver, subnet, gateway)
	case "rm", "remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl network rm <network_name>")
		}
		return commands.NetworkRemove(ctx, c, args[1])
	case "inspect":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl network inspect <network_name>")
		}
		return commands.NetworkInspect(ctx, c, newWriter(), args[1])
	case "wifi":
		return dispatchWifi(ctx, c, args[1:])
	default:
		return fmt.Errorf("unknown network subcommand %q (supported: ps, ls, create, rm, inspect, wifi)", args[0])
	}
}

func dispatchWifi(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 || args[0] == "list" || args[0] == "scan" || args[0] == "ls" || args[0] == "ps" {
		return commands.WifiList(ctx, c, newWriter())
	}
	switch args[0] {
	case "connect", "change", "set":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl wifi connect <ssid> [--password <password>]")
		}
		ssid := args[1]
		var password string
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--password", "-p", "--pass":
				if i+1 < len(args) {
					password = args[i+1]
					i++
				}
			default:
				if password == "" && !strings.HasPrefix(args[i], "-") {
					password = args[i]
				}
			}
		}
		return commands.WifiConnect(ctx, c, ssid, password)
	default:
		return fmt.Errorf("unknown wifi subcommand %q (supported: list, connect)", args[0])
	}
}

func dispatchCreate(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 || args[0] != "network" {
		return fmt.Errorf("usage: nodexactl create network <network_name>")
	}
	name, driver, subnet, gateway, err := parseNetworkCreateArgs(args[1:])
	if err != nil {
		return err
	}
	return commands.NetworkCreate(ctx, c, name, driver, subnet, gateway)
}

func dispatchRm(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nodexactl rm <name> or nodexactl rm network <name>")
	}
	if args[0] == "network" {
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl rm network <network_name>")
		}
		return commands.NetworkRemove(ctx, c, args[1])
	}
	return commands.ContainerRemove(ctx, c, args[0])
}

func parseExecArgs(args []string) (commands.ExecOptions, error) {
	var opts commands.ExecOptions
	i := 0
	for ; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			break
		}
		switch {
		case arg == "-i" || arg == "--interactive":
			opts.Interactive = true
		case arg == "-t" || arg == "--tty":
			opts.TTY = true
		case arg == "-it" || arg == "-ti":
			opts.Interactive = true
			opts.TTY = true
		case arg == "-d" || arg == "--detach":
			opts.Detach = true
		case arg == "-u" || arg == "--user":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag requires an argument: %s", arg)
			}
			opts.User = args[i+1]
			i++
		case arg == "-w" || arg == "--workdir":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("flag requires an argument: %s", arg)
			}
			opts.Workdir = args[i+1]
			i++
		case strings.HasPrefix(arg, "-") && len(arg) > 1 && !strings.HasPrefix(arg, "--"):
			// Handle any combination of short flags, like -it, -di, etc.
			valid := true
			for _, r := range arg[1:] {
				switch r {
				case 'i':
					opts.Interactive = true
				case 't':
					opts.TTY = true
				case 'd':
					opts.Detach = true
				default:
					valid = false
				}
			}
			if !valid {
				return opts, fmt.Errorf("unknown flag: %s", arg)
			}
		default:
			return opts, fmt.Errorf("unknown flag: %s", arg)
		}
	}

	if i >= len(args) {
		return opts, fmt.Errorf("usage: nodexactl exec [flags] <container> [command...]")
	}

	opts.Container = args[i]
	if i+1 < len(args) {
		opts.Command = args[i+1:]
	}

	return opts, nil
}

func dispatchExec(ctx context.Context, c *client.Client, args []string) error {
	opts, err := parseExecArgs(args)
	if err != nil {
		return err
	}
	return commands.ContainerExec(ctx, c, opts)
}

func dispatchUniversalInspect(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nodexactl inspect <name>")
	}
	name := args[0]
	// First check if it's a container
	if ctr, err := c.ContainerInspect(ctx, name); err == nil && ctr != nil {
		return commands.ContainerInspect(ctx, c, newWriter(), name)
	}
	// Next check if it's a network
	if netObj, err := c.NetworkInspect(ctx, name); err == nil && netObj != nil {
		return commands.NetworkInspect(ctx, c, newWriter(), name)
	}
	return fmt.Errorf("no such container or network: %q", name)
}

func dispatchUpdate(ctx context.Context, c *client.Client, args []string) error {
	if len(args) == 0 || args[0] == "status" {
		return commands.UpdateStatus(ctx, c, newWriter())
	}
	switch args[0] {
	case "rollback":
		return commands.UpdateRollback(ctx, c)
	case "apply", "agent":
		if len(args) < 2 {
			return fmt.Errorf("usage: nodexactl update apply <url|file> [--sha256 <hash>] [--version <ver>]")
		}
		target := args[1]
		var sha256Val, versionVal string
		for i := 2; i < len(args); i++ {
			switch args[i] {
			case "--sha256":
				if i+1 < len(args) {
					sha256Val = args[i+1]
					i++
				}
			case "--version":
				if i+1 < len(args) {
					versionVal = args[i+1]
					i++
				}
			}
		}
		return commands.UpdateApply(ctx, c, target, sha256Val, versionVal)
	default:
		if strings.HasPrefix(args[0], "http://") || strings.HasPrefix(args[0], "https://") || strings.HasPrefix(args[0], "/") || strings.HasPrefix(args[0], "./") {
			return commands.UpdateApply(ctx, c, args[0], "", "")
		}
		return fmt.Errorf("unknown update subcommand %q (expected status, apply, or rollback)", args[0])
	}
}


