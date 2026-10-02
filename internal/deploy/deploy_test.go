package deploy

import (
	"testing"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
	"github.com/nodexa/nodexa-os/nodexa-agent/internal/container"
)

func TestReleaseRunning(t *testing.T) {
	dep := &backend.DeploymentResponse{
		Name:     "shop",
		Revision: 3,
		Services: []container.ServiceSpec{
			{Name: "web", Image: "nginx:1.27"},
			{Name: "api", Image: "registry.example.com/api:1.0"},
		},
	}
	running := func(name, image string) container.NodexaContainer {
		return container.NodexaContainer{
			Name:               name,
			Image:              image,
			State:              container.StateRunning,
			DeploymentName:     "shop",
			DeploymentRevision: 3,
		}
	}
	healthy := []container.NodexaContainer{
		running("web", "nginx:1.27"),
		running("api", "registry.example.com/api:1.0"),
	}

	tests := []struct {
		name     string
		dep      *backend.DeploymentResponse
		existing func() []container.NodexaContainer
		want     bool
	}{
		{"all running", dep, func() []container.NodexaContainer { return healthy }, true},
		{"extra unrelated container", dep, func() []container.NodexaContainer {
			return append(append([]container.NodexaContainer{}, healthy...), container.NodexaContainer{Name: "other", State: container.StateRunning})
		}, true},
		{"service missing", dep, func() []container.NodexaContainer { return healthy[:1] }, false},
		{"service stopped", dep, func() []container.NodexaContainer {
			c := append([]container.NodexaContainer{}, healthy...)
			c[1].State = container.StateStopped
			return c
		}, false},
		{"service still pulling", dep, func() []container.NodexaContainer {
			c := append([]container.NodexaContainer{}, healthy...)
			c[0].State = container.StatePullingImage
			return c
		}, false},
		{"older revision", dep, func() []container.NodexaContainer {
			c := append([]container.NodexaContainer{}, healthy...)
			c[0].DeploymentRevision = 2
			return c
		}, false},
		{"different deployment", dep, func() []container.NodexaContainer {
			c := append([]container.NodexaContainer{}, healthy...)
			c[0].DeploymentName = "other"
			return c
		}, false},
		{"different image", dep, func() []container.NodexaContainer {
			c := append([]container.NodexaContainer{}, healthy...)
			c[0].Image = "nginx:1.26"
			return c
		}, false},
		{"no services", &backend.DeploymentResponse{Name: "shop", Revision: 3}, func() []container.NodexaContainer { return healthy }, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := releaseRunning(tt.dep, tt.existing()); got != tt.want {
				t.Fatalf("releaseRunning() = %v, want %v", got, tt.want)
			}
		})
	}
}
