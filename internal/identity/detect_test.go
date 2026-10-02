package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestIsQEMUDetectsFromDMI(t *testing.T) {
	dir := t.TempDir()
	origDMI := dmiRoot
	dmiRoot = dir
	defer func() { dmiRoot = origDMI }()

	writeFixtureFile(t, filepath.Join(dir, "sys_vendor"), "QEMU\n")
	writeFixtureFile(t, filepath.Join(dir, "product_name"), "Standard PC (Q35 + ICH9, 2009)\n")

	if !isQEMU() {
		t.Fatal("expected isQEMU() to detect QEMU DMI strings")
	}
}

func TestIsQEMUFalseOnGenericHardware(t *testing.T) {
	dir := t.TempDir()
	origDMI := dmiRoot
	dmiRoot = dir
	defer func() { dmiRoot = origDMI }()

	writeFixtureFile(t, filepath.Join(dir, "sys_vendor"), "Dell Inc.\n")
	writeFixtureFile(t, filepath.Join(dir, "product_name"), "PowerEdge R640\n")

	if isQEMU() {
		t.Fatal("did not expect isQEMU() to detect generic hardware as QEMU")
	}
}

func TestIsVarisciteDetectsFromDeviceTree(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "compatible")
	writeFixtureFile(t, path, "variscite,var-som-mx8mm\x00fsl,imx8mm\x00")

	origPath := deviceTreeCompatiblePath
	deviceTreeCompatiblePath = path
	defer func() { deviceTreeCompatiblePath = origPath }()

	if !isVariscite() {
		t.Fatal("expected isVariscite() to detect Variscite device tree compatible string")
	}
}

func TestQEMUProviderCollectsHardwareInfo(t *testing.T) {
	dmiDir := t.TempDir()
	netDir := t.TempDir()

	origDMI, origNet := dmiRoot, sysClassNetRoot
	dmiRoot, sysClassNetRoot = dmiDir, netDir
	defer func() { dmiRoot, sysClassNetRoot = origDMI, origNet }()

	writeFixtureFile(t, filepath.Join(dmiDir, "product_uuid"), "11111111-2222-3333-4444-555555555555\n")

	p := &QEMUProvider{}
	info, err := p.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if info["dmi_product_uuid"] == "" {
		t.Fatalf("expected dmi_product_uuid to be collected, got %+v", info)
	}
}
