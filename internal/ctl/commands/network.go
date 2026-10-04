package commands

import (
	"context"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/ctl/client"
)

// Network implements `nodexactl network`.
func Network(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	netInfo, err := c.Network(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintln(out, "Nodexa OS Network")
	fmt.Fprintln(out)
	fmt.Fprintf(out, "Status:\t%s\n", title(netInfo.Status))
	vpnDesc := title(netInfo.VPNStatus)
	if netInfo.TailscaleIP != "" {
		vpnDesc = fmt.Sprintf("%s (%s)", netInfo.TailscaleIP, title(netInfo.VPNStatus))
	}
	fmt.Fprintf(out, "Tailscale VPN:\t%s\n", vpnDesc)
	fmt.Fprintln(out)

	if len(netInfo.Interfaces) == 0 {
		fmt.Fprintln(out, "No network interfaces found.")
		return out.Flush()
	}

	fmt.Fprintln(out, "NAME\tSTATE\tMAC\tADDRESSES")
	for _, iface := range netInfo.Interfaces {
		mac := iface.MAC
		if mac == "" {
			mac = "-"
		}
		addrs := strings.Join(iface.IPAddresses, ", ")
		if addrs == "" {
			addrs = "-"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", iface.Name, iface.State, mac, addrs)
	}

	return out.Flush()
}

// NetworksList implements `nodexactl network ps` / `nodexactl network ls`.
func NetworksList(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	list, err := c.NetworksList(ctx)
	if err != nil {
		return err
	}

	if len(list) == 0 {
		fmt.Fprintln(out, "No container networks found.")
		return out.Flush()
	}

	fmt.Fprintln(out, "NETWORK ID\tNAME\tDRIVER\tSCOPE\tSUBNET\tGATEWAY")
	for _, n := range list {
		subnet := n.Subnet
		if subnet == "" {
			subnet = "-"
		}
		gw := n.Gateway
		if gw == "" {
			gw = "-"
		}
		id := n.ID
		if len(id) > 12 {
			id = id[:12]
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\t%s\n", id, n.Name, n.Driver, n.Scope, subnet, gw)
	}
	return out.Flush()
}

// NetworkCreate implements `nodexactl network create <name>` and `nodexactl create network <name>`.
func NetworkCreate(ctx context.Context, c *client.Client, name, driver, subnet, gateway string) error {
	created, err := c.NetworkCreate(ctx, client.CreateNetworkRequest{
		Name:    name,
		Driver:  driver,
		Subnet:  subnet,
		Gateway: gateway,
	})
	if err != nil {
		return err
	}
	fmt.Println(created.ID)
	fmt.Printf("Network %q created.\n", created.Name)
	return nil
}

// NetworkRemove implements `nodexactl network rm <name>` and `nodexactl rm network <name>`.
func NetworkRemove(ctx context.Context, c *client.Client, name string) error {
	if err := c.NetworkRemove(ctx, name); err != nil {
		return err
	}
	fmt.Printf("Network %q removed.\n", name)
	return nil
}

// NetworkInspect implements `nodexactl network inspect <name>`.
func NetworkInspect(ctx context.Context, c *client.Client, out *tabwriter.Writer, name string) error {
	n, err := c.NetworkInspect(ctx, name)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "Network ID:\t%s\n", n.ID)
	fmt.Fprintf(out, "Name:\t%s\n", n.Name)
	fmt.Fprintf(out, "Driver:\t%s\n", n.Driver)
	fmt.Fprintf(out, "Scope:\t%s\n", n.Scope)
	if n.Subnet != "" {
		fmt.Fprintf(out, "Subnet:\t%s\n", n.Subnet)
	}
	if n.Gateway != "" {
		fmt.Fprintf(out, "Gateway:\t%s\n", n.Gateway)
	}
	if n.Bridge != "" {
		fmt.Fprintf(out, "Bridge Interface:\t%s\n", n.Bridge)
	}
	fmt.Fprintf(out, "Created:\t%s\n", n.CreatedAt.Format(time.RFC3339))
	return out.Flush()
}

// WifiList implements `nodexactl wifi list` and `nodexactl network wifi list`.
func WifiList(ctx context.Context, c *client.Client, out *tabwriter.Writer) error {
	nets, err := c.WifiNetworks(ctx)
	if err != nil {
		return err
	}
	if len(nets) == 0 {
		fmt.Fprintln(out, "No Wi-Fi networks found.")
		return out.Flush()
	}
	fmt.Fprintln(out, "SSID\tSIGNAL\tSECURITY")
	for _, n := range nets {
		fmt.Fprintf(out, "%s\t%d%%\t%s\n", n.SSID, n.SignalPercent, n.Security)
	}
	return out.Flush()
}

// WifiConnect implements `nodexactl wifi connect <ssid> [--password <password>]`.
func WifiConnect(ctx context.Context, c *client.Client, ssid, password string) error {
	resp, err := c.WifiChange(ctx, client.WifiChangeRequest{
		SSID:     ssid,
		Password: password,
	})
	if err != nil {
		return err
	}
	fmt.Printf("Wi-Fi network %q configured successfully (%s).\n", resp.SSID, resp.Status)
	return nil
}

