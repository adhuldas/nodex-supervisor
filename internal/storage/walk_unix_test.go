//go:build unix

package storage

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func write(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

// allocated is what the walk should count for one file: the disk blocks it
// really takes, which is what `du` reports.
func allocated(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Lstat(path, &st); err != nil {
		t.Fatal(err)
	}
	return uint64(st.Blocks) * 512
}

func devOf(t *testing.T, path string) uint64 {
	t.Helper()
	dev, err := deviceOf(path)
	if err != nil {
		t.Fatal(err)
	}
	return dev
}

func dirBlocks(t *testing.T, path string) uint64 { return allocated(t, path) }

func TestWalkSizeCountsAllocatedBlocks(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "one"), 10_000)
	write(t, filepath.Join(root, "a", "b", "two"), 3)

	got, err := walkSize(context.Background(), root, devOf(t, root), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := dirBlocks(t, root) + dirBlocks(t, filepath.Join(root, "a")) + dirBlocks(t, filepath.Join(root, "a", "b")) +
		allocated(t, filepath.Join(root, "a", "one")) + allocated(t, filepath.Join(root, "a", "b", "two"))
	if got != want {
		t.Fatalf("walkSize = %d, want %d", got, want)
	}
	if got < 10_000 {
		t.Fatalf("walkSize = %d, less than the file's own length", got)
	}
}

func TestWalkSizeCountsAHardLinkedFileOnce(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "orig"), 50_000)
	if err := os.Link(filepath.Join(root, "orig"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}

	got, err := walkSize(context.Background(), root, devOf(t, root), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := dirBlocks(t, root) + allocated(t, filepath.Join(root, "orig"))
	if got != want {
		t.Fatalf("walkSize = %d, want %d (the data counted once, not per link)", got, want)
	}
}

func TestWalkSizeDoesNotFollowSymlinks(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "big"), 1_000_000)
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	got, err := walkSize(context.Background(), root, devOf(t, root), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got >= 1_000_000 {
		t.Fatalf("walkSize = %d: counted what a symlink points at", got)
	}
}

func TestWalkSizeSkipsExcludedDirectories(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "keep", "f"), 20_000)
	write(t, filepath.Join(root, "inner", "f"), 90_000)

	got, err := walkSize(context.Background(), root, devOf(t, root), map[string]bool{filepath.Join(root, "inner"): true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := dirBlocks(t, root) + dirBlocks(t, filepath.Join(root, "keep")) + allocated(t, filepath.Join(root, "keep", "f"))
	if got != want {
		t.Fatalf("walkSize = %d, want %d (measured separately, so not counted again)", got, want)
	}
}

func TestWalkSizeStopsWhenCancelled(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < throttleEvery+10; i++ {
		write(t, filepath.Join(root, "d", string(rune('a'+i%26)), string(rune('a'+i/26%26))+"f"), 1)
	}
	for i := 0; i < 2*throttleEvery; i++ {
		write(t, filepath.Join(root, "bulk", "f"+string(rune(0x4e00+i))), 1)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := walkSize(ctx, root, devOf(t, root), nil, nil); err == nil {
		t.Fatal("a cancelled scan returned a result; a partial one would understate everything")
	}
}

func TestWalkSizeCountsADirectoryReachedTwiceOnce(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "data")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), make([]byte, 64*1024), 0o644); err != nil {
		t.Fatal(err)
	}
	dev := devOf(t, root)
	seen := make(map[[2]uint64]struct{})

	first, err := walkSize(context.Background(), dir, dev, nil, seen)
	if err != nil {
		t.Fatal(err)
	}
	// The same directory again, as a bind mount elsewhere would present it.
	again, err := walkSize(context.Background(), dir, dev, nil, seen)
	if err != nil {
		t.Fatal(err)
	}
	if first == 0 || again != 0 {
		t.Fatalf("first = %d, again = %d, want the tree counted once", first, again)
	}
}
