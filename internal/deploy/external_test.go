//go:build !windows

package deploy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/events"
)

// A release on a Docker host must leave the host's own containers and
// images alone: before External, Apply removed every listed container not
// in the release and pruned every image it didn't use.
func TestApplyLeavesExternalDockerContainersAlone(t *testing.T) {
	bin := t.TempDir()
	calls := filepath.Join(t.TempDir(), "calls")
	fake := `#!/bin/sh
echo "$*" >> ` + calls + `
case "$1" in
ps) cat <<'OUT'
{"ID":"a1","Names":"web","Image":"registry.example/web:2","State":"running","Labels":"nodexa.managed=true,nodexa.service=web","CreatedAt":"2026-10-03 09:28:01 +0000 UTC"}
{"ID":"b2","Names":"customer-db","Image":"postgres:16","State":"restarting","Labels":"com.docker.compose.project=shop","CreatedAt":"2026-09-01 08:00:00 +0000 UTC"}
OUT
;;
images) echo '{"Repository":"postgres","Tag":"16","Size":"400MB","CreatedAt":"2026-09-01 08:00:00 +0000 UTC","ID":"p1"}' ;;
inspect) for last; do :; done
	case "$last" in web) echo true ;; customer-db) echo ;; *) exit 1 ;; esac ;;
image) [ "$2" = inspect ] || exit 0; [ "$3" = postgres:16 ] && exit 0; exit 1 ;;
esac
exit 0
`
	if err := os.WriteFile(filepath.Join(bin, "docker"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	mgr := container.NewManager(t.TempDir(), "", t.TempDir(), t.TempDir(), "runc", t.TempDir(), events.NewBus(64))
	mgr.SetEngine(container.EngineDocker)

	list, err := mgr.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list {
		if c.Name == "customer-db" && (!c.External || c.State != container.StateFailed) {
			t.Fatalf("customer-db = %+v, want External and state failed", c)
		}
		if c.Name == "web" && c.External {
			t.Fatalf("web has the nodexa.managed label but was marked External")
		}
	}

	// A service named like the host's container, and one on an image the
	// host already had: neither may cost the host anything.
	dm := NewManager(mgr, events.NewBus(16))
	err = dm.Apply(&backend.DeploymentResponse{
		Name:     "release-1",
		Revision: 1,
		Services: []container.ServiceSpec{
			{Name: "api", Image: "registry.example/api:1"},
			{Name: "customer-db", Image: "registry.example/db:1"},
			{Name: "cache", Image: "postgres:16"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "customer-db") {
		t.Errorf("Apply error = %v, want the customer-db name clash reported", err)
	}
	_ = dm.RemoveAll()

	data, _ := os.ReadFile(calls)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		last := f[len(f)-1]
		destructive := f[0] == "stop" || f[0] == "rm" || f[0] == "rmi"
		if f[0] == "image" && len(f) > 1 && (f[1] == "prune" || f[1] == "rm") {
			t.Errorf("pruned host images: docker %s", line)
		}
		if (destructive && (last == "customer-db" || last == "postgres:16")) ||
			(f[0] == "create" && strings.Contains(line, "--name customer-db ")) {
			t.Errorf("touched the host's own container or images: docker %s", line)
		}
	}
}
