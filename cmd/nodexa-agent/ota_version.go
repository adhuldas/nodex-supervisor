package main

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// otaBinaryVersion asks the OTA binary for its version without starting
// it up: "nodex-supervisor 0.3.8 (commit ..., built ...)", or
// "nodexa-agent 0.3.1 (os ..., commit ...)" from builds before the
// rename. Returns "" when it can't tell.
func otaBinaryVersion(path string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "version").Output()
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 || (fields[0] != "nodex-supervisor" && fields[0] != "nodexa-agent") {
		return ""
	}
	return fields[1]
}

// newerVersion reports whether dotted version a is newer than b. Missing
// or non-numeric parts count as 0 ("0.3" == "0.3.0").
func newerVersion(a, b string) bool {
	pa := strings.Split(strings.TrimPrefix(a, "v"), ".")
	pb := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			return x > y
		}
	}
	return false
}
