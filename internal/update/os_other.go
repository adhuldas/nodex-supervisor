//go:build !linux

package update

import "errors"

func mountFS(source, target, fstype string, readOnly bool) error {
	return errors.New("mount: only supported on linux")
}

func unmountFS(target string) error {
	return errors.New("unmount: only supported on linux")
}
