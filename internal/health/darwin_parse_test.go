package health

import "testing"

func TestDarwinParsers(t *testing.T) {
	vm := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\nPages free:  3698.\nPages active:  216420.\nPages inactive:  209489.\nPages wired down:  171810.\nPages occupied by compressor:  100.\n"
	if used, ok := parseVMStat(vm); !ok || used != (216420+171810+100)*16384 {
		t.Errorf("parseVMStat = %d, %v", used, ok)
	}
	if total, used, ok := parseSwapUsage("total = 12288.00M  used = 11305.12M  free = 982.88M  (encrypted)"); !ok || total != 12288<<20 || used/(1<<20) != 11305 {
		t.Errorf("parseSwapUsage = %d %d %v", total, used, ok)
	}
	if total, _, ok := parseSwapUsage("total = 0.00M  used = 0.00M  free = 0.00M  (encrypted)"); !ok || total != 0 {
		t.Errorf("no swap = %d %v", total, ok)
	}
	if l1, l5, l15, ok := parseLoadAvg("{ 2.08 2.24 2.25 }\n"); !ok || l1 != 2.08 || l5 != 2.24 || l15 != 2.25 {
		t.Errorf("parseLoadAvg = %v %v %v %v", l1, l5, l15, ok)
	}
	if sec, ok := parseBootTime("{ sec = 1790567922, usec = 564922 } Mon Sep 28 09:28:42 2026"); !ok || sec != 1790567922 {
		t.Errorf("parseBootTime = %d %v", sec, ok)
	}
	top := "CPU usage: 60.99% user, 14.37% sys, 24.63% idle \nCPU usage: 60.87% user, 6.23% sys, 32.88% idle\n"
	if pct, ok := parseTopCPU(top); !ok || pct < 67.11 || pct > 67.13 {
		t.Errorf("parseTopCPU = %v %v", pct, ok)
	}
}
