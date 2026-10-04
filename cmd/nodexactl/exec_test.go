package main

import (
	"reflect"
	"testing"
)

func TestParseExecArgs(t *testing.T) {
	tests := []struct {
		name        string
		args        []string
		wantCont    string
		wantCmd     []string
		wantTTY     bool
		wantInt     bool
		wantUser    string
		wantWorkdir string
		wantErr     bool
	}{
		{
			name:     "nodexa exec -it smart-printer-firmware",
			args:     []string{"-it", "smart-printer-firmware"},
			wantCont: "smart-printer-firmware",
			wantCmd:  nil, // defaults to /bin/sh at execution time
			wantTTY:  true,
			wantInt:  true,
		},
		{
			name:     "nodexa exec -it smart-printer-firmware /bin/sh",
			args:     []string{"-it", "smart-printer-firmware", "/bin/sh"},
			wantCont: "smart-printer-firmware",
			wantCmd:  []string{"/bin/sh"},
			wantTTY:  true,
			wantInt:  true,
		},
		{
			name:     "separate -i -t flags",
			args:     []string{"-i", "-t", "my-container", "bash"},
			wantCont: "my-container",
			wantCmd:  []string{"bash"},
			wantTTY:  true,
			wantInt:  true,
		},
		{
			name:        "with user and workdir",
			args:        []string{"-u", "root", "-w", "/etc", "my-container", "cat", "os-release"},
			wantCont:    "my-container",
			wantCmd:     []string{"cat", "os-release"},
			wantUser:    "root",
			wantWorkdir: "/etc",
		},
		{
			name:     "missing container name",
			args:     []string{"-it"},
			wantErr:  true,
		},
		{
			name:     "no args",
			args:     []string{},
			wantErr:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseExecArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseExecArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if opts.Container != tt.wantCont {
				t.Errorf("Container = %q, want %q", opts.Container, tt.wantCont)
			}
			if !reflect.DeepEqual(opts.Command, tt.wantCmd) {
				t.Errorf("Command = %v, want %v", opts.Command, tt.wantCmd)
			}
			if opts.TTY != tt.wantTTY {
				t.Errorf("TTY = %v, want %v", opts.TTY, tt.wantTTY)
			}
			if opts.Interactive != tt.wantInt {
				t.Errorf("Interactive = %v, want %v", opts.Interactive, tt.wantInt)
			}
			if opts.User != tt.wantUser {
				t.Errorf("User = %q, want %q", opts.User, tt.wantUser)
			}
			if opts.Workdir != tt.wantWorkdir {
				t.Errorf("Workdir = %q, want %q", opts.Workdir, tt.wantWorkdir)
			}
		})
	}
}
