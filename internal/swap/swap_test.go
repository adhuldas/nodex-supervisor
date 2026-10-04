package swap

import "testing"

func TestWithFstabEntry(t *testing.T) {
	for in, want := range map[string]string{
		"":                                    "/swapfile none swap sw 0 0\n",
		"UUID=abc / ext4 defaults 0 1":        "UUID=abc / ext4 defaults 0 1\n/swapfile none swap sw 0 0\n",
		"UUID=abc / ext4 defaults 0 1\n":      "UUID=abc / ext4 defaults 0 1\n/swapfile none swap sw 0 0\n",
		"/swapfile  none swap defaults 0 0\n": "/swapfile  none swap defaults 0 0\n",
		"/swapfile2 none swap sw 0 0\n":       "/swapfile2 none swap sw 0 0\n/swapfile none swap sw 0 0\n",
	} {
		if got := withFstabEntry(in); got != want {
			t.Errorf("withFstabEntry(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsActive(t *testing.T) {
	procSwaps := "Filename\tType\tSize\tUsed\tPriority\n/dev/sda2 partition 1048572 0 -2\n/swapfile file 2097148 0 -3\n"
	if !isActive(procSwaps) {
		t.Error("isActive missed /swapfile")
	}
	if isActive("Filename\tType\tSize\tUsed\tPriority\n/swapfile2 file 1 0 -2\n") {
		t.Error("isActive matched /swapfile2")
	}
}

func TestValidateSize(t *testing.T) {
	for size, ok := range map[int]bool{0: false, 255: false, 256: true, 4096: true, 65536: true, 65537: false} {
		if err := validateSize(size); (err == nil) != ok {
			t.Errorf("validateSize(%d) = %v", size, err)
		}
	}
}
