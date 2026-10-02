package wifi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyfileWPA(t *testing.T) {
	got := Keyfile(Credentials{SSID: "Home;Net", Password: `pa ss\word`})
	for _, want := range []string{
		"id=nodexa-wifi\n",
		"type=wifi\n",
		"ssid=72;111;109;101;59;78;101;116;\n",
		"key-mgmt=wpa-psk\n",
		`psk=pa\sss\\word` + "\n",
		"route-metric=2048\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("keyfile missing %q:\n%s", want, got)
		}
	}
}

func TestKeyfileOpenNetwork(t *testing.T) {
	got := Keyfile(Credentials{SSID: "Cafe"})
	if strings.Contains(got, "[wifi-security]") {
		t.Errorf("open network got a security section:\n%s", got)
	}
}

func TestKeyfileUUIDStable(t *testing.T) {
	a := connectionUUID("Home")
	if a != connectionUUID("Home") {
		t.Fatal("uuid not stable")
	}
	if a == connectionUUID("Other") {
		t.Fatal("different SSIDs share a uuid")
	}
	if len(a) != 36 || a[14] != '5' {
		t.Fatalf("not a version 5 uuid: %s", a)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		c  Credentials
		ok bool
	}{
		{Credentials{SSID: "Home", Password: "12345678"}, true},
		{Credentials{SSID: "Home"}, true},
		{Credentials{SSID: ""}, false},
		{Credentials{SSID: strings.Repeat("x", 33)}, false},
		{Credentials{SSID: "Home", Password: "short"}, false},
		{Credentials{SSID: "Home", Password: "1234567\n8"}, false},
	} {
		if err := tc.c.Validate(); (err == nil) != tc.ok {
			t.Errorf("Validate(%+v) = %v, want ok=%v", tc.c, err, tc.ok)
		}
	}
}

func TestApplyWritesOnlyOnChange(t *testing.T) {
	nmcliPath = "nodexa-test-no-such-nmcli"
	dir := filepath.Join(t.TempDir(), "system-connections")
	c := Credentials{SSID: "Home", Password: "12345678"}

	changed, err := Apply(context.Background(), dir, c)
	if err != nil || !changed {
		t.Fatalf("first Apply = %v, %v", changed, err)
	}
	path := filepath.Join(dir, "nodexa-wifi.nmconnection")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("keyfile mode %v, want 0600", info.Mode().Perm())
	}

	if changed, err := Apply(context.Background(), dir, c); err != nil || changed {
		t.Fatalf("repeat Apply = %v, %v, want unchanged", changed, err)
	}

	if err := Remove(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("keyfile still present: %v", err)
	}
	if err := Remove(context.Background(), dir); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
}

func TestApplyRejectsInvalid(t *testing.T) {
	dir := t.TempDir()
	if _, err := Apply(context.Background(), dir, Credentials{SSID: "Home", Password: "short"}); err == nil {
		t.Fatal("expected error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("wrote files for invalid credentials: %v", entries)
	}
}
