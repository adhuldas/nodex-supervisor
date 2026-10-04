package update

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/nodexa/nodexa-os/nodexa-agent/internal/backend"
)

// makeDiskImage builds a tiny MBR image: p1 at sector 2 (2 sectors),
// p2 at sector 4 holding root.
func makeDiskImage(root []byte) []byte {
	rootSectors := (len(root) + sectorSize - 1) / sectorSize
	img := make([]byte, (4+rootSectors)*sectorSize)
	entry := func(n int, start, size uint32) {
		e := img[446+(n-1)*16:]
		e[4] = 0x83
		binary.LittleEndian.PutUint32(e[8:], start)
		binary.LittleEndian.PutUint32(e[12:], size)
	}
	entry(1, 2, 2)
	entry(2, 4, uint32(rootSectors))
	img[510], img[511] = 0x55, 0xaa
	copy(img[2*sectorSize:], bytes.Repeat([]byte{'B'}, 2*sectorSize))
	copy(img[4*sectorSize:], root)
	return img
}

func zipBytes(t *testing.T, name string, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(data)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func gzipBytes(data []byte) []byte {
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write(data)
	gw.Close()
	return buf.Bytes()
}

// osTestEnv fakes /proc, /sys and /dev for a device booted from mmcblk1p<slot>.
type osTestEnv struct {
	u        *OSUpdater
	devDir   string
	rebooted bool
	// newRootVersion is what the fake mount of the new slot reports.
	newRootVersion string
	// newKernel is the kernel in the new slot's /boot (default zImage).
	newKernel string
}

func newOSTestEnv(t *testing.T, slot, osVersion string, slotBytes int64) *osTestEnv {
	t.Helper()
	root := t.TempDir()
	proc := filepath.Join(root, "proc")
	sys := filepath.Join(root, "sys")
	dev := filepath.Join(root, "dev")
	etc := filepath.Join(root, "etc")
	for _, d := range []string{filepath.Join(proc, "self"), dev, etc, filepath.Join(sys, "dev", "block")} {
		os.MkdirAll(d, 0o755)
	}
	os.WriteFile(filepath.Join(proc, "cmdline"), []byte("console=ttyS0,115200 root=PARTUUID=x rootwait ro nodexa.slot="+slot+"\n"), 0o644)
	os.WriteFile(filepath.Join(etc, "os-release"), []byte("ID=nodexa\nVERSION_ID=\""+osVersion+"\"\n"), 0o644)

	rootNum := slotPartition(slot)
	os.WriteFile(filepath.Join(proc, "self", "mountinfo"), []byte(
		"1 0 179:"+string(rune('0'+rootNum))+" / / ro,relatime - ext4 /dev/root ro\n"+
			"2 1 0:5 / /dev rw - devtmpfs devtmpfs rw\n"), 0o644)

	for n := 1; n <= 3; n++ {
		name := partitionName("mmcblk1", n)
		p := filepath.Join(sys, "block", "mmcblk1", name)
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, "partition"), []byte{byte('0' + n), '\n'}, 0o644)
		os.Symlink(p, filepath.Join(sys, "dev", "block", "179:"+string(rune('0'+n))))
		cls := filepath.Join(sys, "class", "block", name)
		os.MkdirAll(cls, 0o755)
		sectors := slotBytes / sectorSize
		os.WriteFile(filepath.Join(cls, "size"), []byte(strconv.FormatInt(sectors, 10)+"\n"), 0o644)
		os.WriteFile(filepath.Join(dev, name), make([]byte, slotBytes), 0o644)
	}

	env := &osTestEnv{devDir: dev, newKernel: "zImage"}
	u := NewOSUpdater(filepath.Join(root, "data"), filepath.Join(root, "run"), nil)
	u.procDir, u.sysDir, u.devDir = proc, sys, dev
	u.osReleasePath = filepath.Join(etc, "os-release")
	u.bootDir = filepath.Join(root, "boot")
	u.mount = func(source, target, fstype string, readOnly bool) error {
		if fstype == "ext4" {
			os.MkdirAll(filepath.Join(target, "etc"), 0o755)
			os.MkdirAll(filepath.Join(target, "boot"), 0o755)
			os.MkdirAll(filepath.Join(target, "usr", "sbin"), 0o755)
			os.WriteFile(filepath.Join(target, "etc", "os-release"), []byte("VERSION_ID="+env.newRootVersion+"\n"), 0o644)
			os.WriteFile(filepath.Join(target, "boot", env.newKernel), []byte("k"), 0o644)
			os.WriteFile(filepath.Join(target, "usr", "sbin", "nodexa-os-commit"), []byte("#!/bin/sh"), 0o755)
		}
		return nil
	}
	u.unmount = func(string) error { return nil }
	u.reboot = func() error { env.rebooted = true; return nil }
	env.u = u
	return env
}

func serve(t *testing.T, body []byte) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func TestOSUpdaterApplyWritesInactiveSlot(t *testing.T) {
	for _, tc := range []struct {
		name string
		wrap func(t *testing.T, img []byte) []byte
	}{
		{"zip", func(t *testing.T, img []byte) []byte { return zipBytes(t, "nodexa-os.wic", img) }},
		{"gzip", func(t *testing.T, img []byte) []byte { return gzipBytes(img) }},
		{"raw", func(t *testing.T, img []byte) []byte { return img }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newOSTestEnv(t, "a", "1.0.0", 64*sectorSize)
			env.newRootVersion = "1.1.0"
			rootfs := bytes.Repeat([]byte("rootfs-1.1.0!"), 300)
			artifact := tc.wrap(t, makeDiskImage(rootfs))
			srv := serve(t, artifact)

			target := backend.AgentUpdateTarget{Version: "1.1.0", URL: srv.URL + "/os.wic.zip", SHA256: sha(artifact)}
			if !env.u.ShouldApply(target) {
				t.Fatal("ShouldApply = false for a new version")
			}
			var states []string
			if err := env.u.Apply(context.Background(), target, func(s string, _ int, _ error) { states = append(states, s) }); err != nil {
				t.Fatalf("Apply: %v", err)
			}
			if !env.rebooted {
				t.Fatal("expected reboot")
			}
			if states[len(states)-1] != "rebooting" {
				t.Fatalf("last state = %q, want rebooting (%v)", states[len(states)-1], states)
			}

			slotB, _ := os.ReadFile(filepath.Join(env.devDir, "mmcblk1p3"))
			if !bytes.HasPrefix(slotB, rootfs) {
				t.Fatal("slot b does not hold the image's root partition")
			}
			slotA, _ := os.ReadFile(filepath.Join(env.devDir, "mmcblk1p2"))
			if !bytes.Equal(slotA, make([]byte, len(slotA))) {
				t.Fatal("running slot a was modified")
			}
			bootEnv, err := os.ReadFile(filepath.Join(env.u.mountDir, "boot", bootEnvName))
			if err != nil {
				t.Fatal(err)
			}
			if string(bootEnv) != "nodexa_slot=b\nnodexa_upgrade=1\nnodexa_tries=0\n" {
				t.Fatalf("nodexa.env = %q", bootEnv)
			}
		})
	}
}

func TestOSUpdaterApplyRejectsWrongVersionImage(t *testing.T) {
	env := newOSTestEnv(t, "b", "1.0.0", 64*sectorSize)
	env.newRootVersion = "0.9.0"
	artifact := makeDiskImage([]byte("old"))
	srv := serve(t, artifact)

	err := env.u.Apply(context.Background(), backend.AgentUpdateTarget{Version: "1.1.0", URL: srv.URL}, nil)
	if err == nil || !strings.Contains(err.Error(), "not the pinned") {
		t.Fatalf("err = %v, want version mismatch", err)
	}
	if env.rebooted {
		t.Fatal("rebooted after a failed update")
	}
	if _, err := os.Stat(filepath.Join(env.u.mountDir, "boot", bootEnvName)); err == nil {
		t.Fatal("boot slot switched after a failed update")
	}
	if env.u.ShouldApply(backend.AgentUpdateTarget{Version: "1.1.0"}) {
		t.Fatal("failed version retried immediately")
	}
	if st := env.u.HeartbeatStatus(); st != nil {
		t.Fatalf("HeartbeatStatus = %+v after Apply returned", st)
	}
}

func TestOSUpdaterApplyKeepsGrubEnvFormat(t *testing.T) {
	env := newOSTestEnv(t, "a", "1.0.0", 64*sectorSize)
	env.newRootVersion = "1.1.0"
	env.newKernel = "bzImage"
	artifact := makeDiskImage([]byte("rootfs"))
	srv := serve(t, artifact)

	bootDir := filepath.Join(env.u.mountDir, "boot")
	os.MkdirAll(bootDir, 0o755)
	os.WriteFile(filepath.Join(bootDir, bootEnvName), grubEnvBlock([]byte(formatBootEnv("a", false))), 0o644)

	if err := env.u.Apply(context.Background(), backend.AgentUpdateTarget{Version: "1.1.0", URL: srv.URL}, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	bootEnv, _ := os.ReadFile(filepath.Join(bootDir, bootEnvName))
	if len(bootEnv) != grubEnvSize {
		t.Fatalf("nodexa.env is %d bytes, want a %d-byte GRUB environment block", len(bootEnv), grubEnvSize)
	}
	want := grubEnvHeader + "nodexa_slot=b\nnodexa_upgrade=1\nnodexa_tries=0\n#"
	if !strings.HasPrefix(string(bootEnv), want) || strings.TrimRight(string(bootEnv[len(want):]), "#") != "" {
		t.Fatalf("nodexa.env = %q", bootEnv)
	}
}

func TestOSUpdaterApplyWritesGrubEnvWhenGrubDetectedEvenIfOldEnvWasPlain(t *testing.T) {
	env := newOSTestEnv(t, "a", "1.0.0", 64*sectorSize)
	env.newRootVersion = "1.1.0"
	env.newKernel = "bzImage"
	artifact := makeDiskImage([]byte("rootfs"))
	srv := serve(t, artifact)

	bootDir := filepath.Join(env.u.mountDir, "boot")
	os.MkdirAll(filepath.Join(bootDir, "EFI", "BOOT"), 0o755)
	os.WriteFile(filepath.Join(bootDir, "EFI", "BOOT", "grub.cfg"), []byte("# grub cfg"), 0o644)
	// Existing nodexa.env is plain text (without GRUB header)
	os.WriteFile(filepath.Join(bootDir, bootEnvName), []byte(formatBootEnv("a", false)), 0o644)

	if err := env.u.Apply(context.Background(), backend.AgentUpdateTarget{Version: "1.1.0", URL: srv.URL}, nil); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	bootEnv, _ := os.ReadFile(filepath.Join(bootDir, bootEnvName))
	if len(bootEnv) != grubEnvSize {
		t.Fatalf("nodexa.env is %d bytes, want a %d-byte GRUB environment block", len(bootEnv), grubEnvSize)
	}
	want := grubEnvHeader + "nodexa_slot=b\nnodexa_upgrade=1\nnodexa_tries=0\n#"
	if !strings.HasPrefix(string(bootEnv), want) {
		t.Fatalf("nodexa.env = %q, want prefix %q", bootEnv, want)
	}
}

func TestOSUpdaterApplyRejectsOtherDeviceTypeImage(t *testing.T) {
	env := newOSTestEnv(t, "a", "1.0.0", 64*sectorSize)
	env.newRootVersion = "1.1.0"
	env.newKernel = "zImage"
	os.MkdirAll(env.u.bootDir, 0o755)
	os.WriteFile(filepath.Join(env.u.bootDir, "bzImage"), []byte("k"), 0o644)
	srv := serve(t, makeDiskImage([]byte("arm rootfs")))

	err := env.u.Apply(context.Background(), backend.AgentUpdateTarget{Version: "1.1.0", URL: srv.URL}, nil)
	if err == nil || !strings.Contains(err.Error(), "different device type") {
		t.Fatalf("err = %v, want device type mismatch", err)
	}
	if env.rebooted {
		t.Fatal("rebooted after a failed update")
	}
	if _, err := os.Stat(filepath.Join(env.u.mountDir, "boot", bootEnvName)); err == nil {
		t.Fatal("boot slot switched after a failed update")
	}
}

func TestOSUpdaterRetryReusesDownload(t *testing.T) {
	env := newOSTestEnv(t, "a", "1.0.0", 64*sectorSize)
	env.newRootVersion = "0.9.0" // first attempt fails after downloading
	artifact := makeDiskImage(bytes.Repeat([]byte("rootfs-1.1.0!"), 300))
	var downloads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downloads++
		w.Write(artifact)
	}))
	t.Cleanup(srv.Close)
	target := backend.AgentUpdateTarget{Version: "1.1.0", URL: srv.URL, SHA256: sha(artifact)}

	if err := env.u.Apply(context.Background(), target, nil); err == nil {
		t.Fatal("first Apply succeeded, want version mismatch")
	}
	env.newRootVersion = "1.1.0"
	if err := env.u.Apply(context.Background(), target, nil); err != nil {
		t.Fatalf("second Apply: %v", err)
	}
	if downloads != 1 {
		t.Fatalf("downloaded %d times, want 1", downloads)
	}
	if left, _ := filepath.Glob(filepath.Join(env.u.workDir, osImagePrefix+"*")); len(left) != 0 {
		t.Fatalf("image kept after a successful install: %v", left)
	}
}

func TestOSUpdaterApplyChecksumMismatch(t *testing.T) {
	env := newOSTestEnv(t, "a", "1.0.0", 64*sectorSize)
	srv := serve(t, makeDiskImage([]byte("x")))
	err := env.u.Apply(context.Background(), backend.AgentUpdateTarget{Version: "1.1.0", URL: srv.URL, SHA256: strings.Repeat("0", 64)}, nil)
	if err == nil || !strings.Contains(err.Error(), "sha256 mismatch") {
		t.Fatalf("err = %v, want sha256 mismatch", err)
	}
}

func TestOSUpdaterApplyRootTooLarge(t *testing.T) {
	env := newOSTestEnv(t, "a", "1.0.0", 4*sectorSize)
	srv := serve(t, makeDiskImage(make([]byte, 8*sectorSize)))
	err := env.u.Apply(context.Background(), backend.AgentUpdateTarget{Version: "1.1.0", URL: srv.URL}, nil)
	if err == nil || !strings.Contains(err.Error(), "larger than slot") {
		t.Fatalf("err = %v, want size error", err)
	}
}

func TestOSUpdaterNeedsABBoot(t *testing.T) {
	env := newOSTestEnv(t, "a", "1.0.0", 64*sectorSize)
	os.WriteFile(filepath.Join(env.u.procDir, "cmdline"), []byte("root=/dev/mmcblk1p2 rootwait\n"), 0o644)
	err := env.u.Apply(context.Background(), backend.AgentUpdateTarget{Version: "1.1.0", URL: "http://x"}, nil)
	if err != ErrNoABLayout {
		t.Fatalf("err = %v, want ErrNoABLayout", err)
	}
}

func TestOSUpdaterResume(t *testing.T) {
	t.Run("completed", func(t *testing.T) {
		env := newOSTestEnv(t, "b", "1.1.0", 64*sectorSize)
		os.MkdirAll(filepath.Dir(env.u.stateFile), 0o755)
		env.u.saveState(&osUpdateState{Version: "1.1.0", FromSlot: "a", TargetSlot: "b"})
		p := env.u.Resume()
		if p == nil || p.State != "completed" {
			t.Fatalf("Resume = %+v, want completed", p)
		}
		if hb := env.u.HeartbeatStatus(); hb == nil || hb.State != "completed" {
			t.Fatalf("HeartbeatStatus = %+v", hb)
		}
		env.u.Reported()
		if hb := env.u.HeartbeatStatus(); hb != nil {
			t.Fatalf("HeartbeatStatus after Reported = %+v", hb)
		}
		if env.u.Resume() != nil {
			t.Fatal("Resume reported the same outcome twice")
		}

		// The new slot then fails before being committed; boot.scr goes back to a.
		os.WriteFile(filepath.Join(env.u.procDir, "cmdline"), []byte("nodexa.slot=a\n"), 0o644)
		if p := env.u.Resume(); p == nil || p.State != "failed" {
			t.Fatalf("Resume after late rollback = %+v, want failed", p)
		}
	})
	t.Run("rolled back", func(t *testing.T) {
		env := newOSTestEnv(t, "a", "1.0.0", 64*sectorSize)
		os.MkdirAll(filepath.Dir(env.u.stateFile), 0o755)
		env.u.saveState(&osUpdateState{Version: "1.1.0", FromSlot: "a", TargetSlot: "b"})
		p := env.u.Resume()
		if p == nil || p.State != "failed" || !strings.Contains(p.Error, "rolled back") {
			t.Fatalf("Resume = %+v, want rolled-back failure", p)
		}
		if env.u.ShouldApply(backend.AgentUpdateTarget{Version: "1.1.0"}) {
			t.Fatal("a rolled-back version must not be reinstalled automatically")
		}
		if !env.u.ShouldApply(backend.AgentUpdateTarget{Version: "1.2.0"}) {
			t.Fatal("a newer version should still be installable")
		}
	})
}

func TestOSUpdaterCurrentVersionAndSlot(t *testing.T) {
	env := newOSTestEnv(t, "b", "2.3.4", 64*sectorSize)
	if v := env.u.CurrentVersion(); v != "2.3.4" {
		t.Fatalf("CurrentVersion = %q", v)
	}
	if s := env.u.CurrentSlot(); s != "b" {
		t.Fatalf("CurrentSlot = %q", s)
	}
	if env.u.ShouldApply(backend.AgentUpdateTarget{Version: "2.3.4"}) {
		t.Fatal("ShouldApply = true for the running version")
	}
}

func TestPartitionName(t *testing.T) {
	for disk, want := range map[string]string{"mmcblk1": "mmcblk1p3", "sda": "sda3", "nvme0n1": "nvme0n1p3"} {
		if got := partitionName(disk, 3); got != want {
			t.Errorf("partitionName(%q) = %q, want %q", disk, got, want)
		}
	}
}

func TestParseSlot(t *testing.T) {
	for in, want := range map[string]string{
		"root=x nodexa.slot=a ro": "a",
		"nodexa.slot=b":           "b",
		"nodexa.slot=c":           "",
		"root=/dev/mmcblk1p2":     "",
	} {
		if got := parseSlot(in); got != want {
			t.Errorf("parseSlot(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestOpenDiskImageRejectsMultiFileZip(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range []string{"a.wic", "b.wic"} {
		w, _ := zw.Create(n)
		w.Write([]byte("x"))
	}
	zw.Close()
	p := filepath.Join(t.TempDir(), "img.zip")
	os.WriteFile(p, buf.Bytes(), 0o644)
	if _, _, err := openDiskImage(p); err == nil {
		t.Fatal("expected an error for a zip with two images")
	}
}

func TestOSUpdaterSkipsOlderVersion(t *testing.T) {
	env := newOSTestEnv(t, "a", "0.1.10", 64*sectorSize)
	if env.u.ShouldApply(backend.AgentUpdateTarget{Version: "0.1.9"}) {
		t.Fatal("ShouldApply = true for a version older than the running one")
	}
	p := env.u.HeartbeatStatus()
	if p == nil || p.State != "skipped" || p.Version != "0.1.9" || !strings.Contains(p.Error, "0.1.10") {
		t.Fatalf("HeartbeatStatus = %+v, want a skipped report", p)
	}
	env.u.Reported()
	env.u.ShouldApply(backend.AgentUpdateTarget{Version: "0.1.9"})
	if p := env.u.HeartbeatStatus(); p != nil {
		t.Fatalf("skipped reported again: %+v", p)
	}
	if !env.u.ShouldApply(backend.AgentUpdateTarget{Version: "0.1.11"}) {
		t.Fatal("a newer version should still be installable")
	}
}

func TestOlderVersion(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"0.1.2", "0.1.3", true}, {"0.1.3", "0.1.2", false}, {"0.1.3", "0.1.3", false},
		{"0.1.9", "0.1.10", true}, {"0.1", "0.1.1", true}, {"1.0", "0.9.9", false},
		{"0.1.0-rc1", "0.1.3", false}, {"dev", "0.1.3", false}, {"", "0.1", false},
	} {
		if got := olderVersion(tc.a, tc.b); got != tc.want {
			t.Errorf("olderVersion(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestRunningOSVersion(t *testing.T) {
	v := RunningOSVersion()
	if v == "" {
		t.Fatal("RunningOSVersion() returned empty string")
	}
	if runtime.GOOS == "darwin" {
		t.Logf("RunningOSVersion() on macOS = %q", v)
		if v == "0.3.7" {
			t.Fatalf("RunningOSVersion() returned default version 0.3.7 instead of macOS version")
		}
	}
}
