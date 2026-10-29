package msucollect

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDiscoverQEMUPIDsIn(t *testing.T) {
	root := t.TempDir()
	mk := func(name, comm string) {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if comm != "" {
			if err := os.WriteFile(filepath.Join(dir, "comm"), []byte(comm), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("89708", "qemu-system-x86\n") // comm truncated to 15 chars by the kernel
	mk("4242", "qemu-system-aar\n")
	mk("1", "init\n")
	mk("77", "vhost-89708\n")
	mk("300", "") // no comm file (process exited)
	mk("sys", "qemu-system-x86\n")
	if err := os.WriteFile(filepath.Join(root, "uptime"), []byte("1 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := discoverQEMUPIDsIn(root)
	want := []int{4242, 89708}
	if len(got) != len(want) {
		t.Fatalf("pids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pids = %v, want %v", got, want)
		}
	}
	if got := discoverQEMUPIDsIn(filepath.Join(root, "missing")); got != nil {
		t.Fatalf("missing root: got %v, want nil", got)
	}
}
