// Package version carries build-time version metadata for nodexa-agent.
//
// Values are populated via -ldflags at build time (see the Yocto recipe in
// layers/meta-nodexa/recipes-nodexa/nodexa-agent, and scripts/build.sh for
// native builds). Sensible defaults are provided for local `go build`.
package version

// These variables are overridden at link time with:
//
//	-X github.com/nodexa/nodexa-os/nodexa-agent/internal/version.OSVersion=0.1.5
//	-X .../version.AgentVersion=0.1.5
//	-X .../version.Commit=<git-sha>
//	-X .../version.BuildDate=<RFC3339 date>
var (
	// OSVersion is the Nodexa OS release version (from the repository VERSION file).
	OSVersion = "0.1.5"
	// AgentVersion is the nodexa-agent component version.
	AgentVersion = "0.1.5"
	// Commit is the git commit SHA the binary was built from.
	Commit = "unknown"
	// BuildDate is the UTC build timestamp in RFC3339 form.
	BuildDate = "unknown"
)

// Info is a structured snapshot of version metadata, suitable for JSON
// serialization over the agent API.
type Info struct {
	OSVersion    string `json:"os_version"`
	AgentVersion string `json:"agent_version"`
	Commit       string `json:"build_commit"`
	BuildDate    string `json:"build_date"`
	Architecture string `json:"architecture"`
}
