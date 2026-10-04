package commands

import "strings"

// agentLabel renders an agent health.Status value the way `nodexactl
// status` displays it.
func agentLabel(status string) string {
	switch status {
	case "healthy":
		return "Healthy"
	case "degraded":
		return "Degraded"
	default:
		return "Unhealthy"
	}
}

// runtimeLabel renders the container runtime status.
func runtimeLabel(status string) string {
	if status == "healthy" {
		return "Ready"
	}
	return "Not Ready"
}

// networkLabel renders the network status.
func networkLabel(status string) string {
	if status == "healthy" {
		return "Connected"
	}
	return "Disconnected"
}

func title(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
