package vpn

import (
	"reflect"
	"strings"
	"testing"
)

const nonDefaultFlagsError = `Error: changing settings via 'tailscale up' requires mentioning all
non-default flags. To proceed, either re-run your command with --reset or
use the command below to explicitly mention the current value of
all non-default settings:

	tailscale up --accept-dns=false --auth-key=tskey-auth-abc123CNTRL-xyz --hostname=d9c2 --ssh --accept-routes
`

func TestKeepSettingsArgs(t *testing.T) {
	got := keepSettingsArgs(nonDefaultFlagsError)
	want := []string{"up", "--accept-dns=false", "--auth-key=tskey-auth-abc123CNTRL-xyz", "--hostname=d9c2", "--ssh", "--accept-routes"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("keepSettingsArgs = %q", got)
	}
	if keepSettingsArgs("tailscale up --ok; rm -rf /") != nil {
		t.Fatal("ran a suggested command with a non-flag argument")
	}
	if keepSettingsArgs("some other failure") != nil {
		t.Fatal("found a command where there is none")
	}
}

func TestRedactKeys(t *testing.T) {
	got := redactKeys(nonDefaultFlagsError)
	if strings.Contains(got, "abc123") || !strings.Contains(got, "tskey-[redacted]") {
		t.Fatalf("auth key not redacted: %s", got)
	}
}

func TestWithoutArg(t *testing.T) {
	got := withoutArg([]string{"up", "--ssh", "--hostname=x", "--ssh=true"}, "--ssh")
	if !reflect.DeepEqual(got, []string{"up", "--hostname=x"}) {
		t.Fatalf("withoutArg = %q", got)
	}
}
