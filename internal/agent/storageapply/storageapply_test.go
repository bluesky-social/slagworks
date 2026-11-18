//go:build !integration

package storageapply

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bluesky-social/slagworks/internal/storage"
)

// TestResolveStorageLayout_MachineIDPath covers the normal deploy path
// (machineID != ""). The contract that matters for the pop1-kworker bug:
// a 204 (no script matched) or a fetch error must HARD-FAIL, not fall
// back to a default layout that picks an arbitrary disk.
func TestResolveStorageLayout_MachineIDPath(t *testing.T) {
	twoDisks := []storage.BlockDevice{
		{Name: "nvme0n1", SizeBytes: 1 << 40, Transport: "nvme", Model: "DATA"},
		{Name: "nvme1n1", SizeBytes: 1 << 40, Transport: "nvme", Model: "DATA"},
	}

	t.Run("204 no script matched -> error, no fallback", func(t *testing.T) {
		fetch := func(url string) ([]byte, error) { return nil, nil } // 204 shape
		_, err := ResolveLayout(testCtx("site:8080"), "machine-abc", twoDisks, fetch, emptyInventory)
		if err == nil {
			t.Fatal("expected error when no storage script matched, got nil (silent fallback is the bug)")
		}
		if !strings.Contains(err.Error(), "no storage script matched") {
			t.Errorf("error should explain the no-match condition, got: %v", err)
		}
	})

	t.Run("empty body -> error, no fallback", func(t *testing.T) {
		fetch := func(url string) ([]byte, error) { return []byte{}, nil }
		_, err := ResolveLayout(testCtx("site:8080"), "machine-abc", twoDisks, fetch, emptyInventory)
		if err == nil {
			t.Fatal("expected error on empty script body, got nil")
		}
	})

	t.Run("fetch error -> error, no fallback", func(t *testing.T) {
		fetch := func(url string) ([]byte, error) {
			return nil, fmt.Errorf("connection refused")
		}
		_, err := ResolveLayout(testCtx("site:8080"), "machine-abc", twoDisks, fetch, emptyInventory)
		if err == nil {
			t.Fatal("expected error when fetch fails, got nil")
		}
		if !strings.Contains(err.Error(), "fetching storage script") {
			t.Errorf("error should mention the fetch failure, got: %v", err)
		}
	})

	t.Run("fetch URL is machine-scoped", func(t *testing.T) {
		var gotURL string
		fetch := func(url string) ([]byte, error) {
			gotURL = url
			return nil, nil
		}
		_, _ = ResolveLayout(testCtx("site:8080"), "machine-abc", twoDisks, fetch, emptyInventory)
		want := "http://site:8080/api/v1/machines/machine-abc/storage-script"
		if gotURL != want {
			t.Errorf("fetch URL = %q, want %q", gotURL, want)
		}
	})
}

// TestResolveStorageLayout_BootstrapPath covers machineID == "" — the
// MAC-based bootstrap path that legitimately has no per-machine script.
// It returns a default layout on a single-disk host but REFUSES to guess
// on a multi-disk host.
func TestResolveStorageLayout_BootstrapPath(t *testing.T) {
	// fetch must never be called on the bootstrap path.
	failFetch := func(url string) ([]byte, error) {
		t.Helper()
		t.Fatalf("fetch must not be called when machineID is empty (got url %q)", url)
		return nil, nil
	}

	t.Run("single disk -> default layout", func(t *testing.T) {
		disks := []storage.BlockDevice{{Name: "vda", SizeBytes: 1 << 40}}
		layout, err := ResolveLayout(testCtx("site:8080"), "", disks, failFetch, emptyInventory)
		if err != nil {
			t.Fatalf("single-disk bootstrap should succeed, got: %v", err)
		}
		if got := layout.Mounts["/"].Device; got != "vda2" {
			t.Errorf("root device = %q, want vda2", got)
		}
	})

	t.Run("multi disk -> refuse", func(t *testing.T) {
		disks := []storage.BlockDevice{
			{Name: "nvme0n1", SizeBytes: 1 << 40},
			{Name: "nvme1n1", SizeBytes: 1 << 40},
		}
		_, err := ResolveLayout(testCtx("site:8080"), "", disks, failFetch, emptyInventory)
		if err == nil {
			t.Fatal("multi-disk bootstrap must refuse, got nil error")
		}
		if !strings.Contains(err.Error(), "refusing to auto-select") {
			t.Errorf("error should explain the refusal, got: %v", err)
		}
	})
}

func TestBootstrapDefaultLayout(t *testing.T) {
	t.Run("zero disks -> error", func(t *testing.T) {
		_, err := BootstrapDefaultLayout(nil)
		if err == nil {
			t.Fatal("expected error on zero disks")
		}
	})

	t.Run("one disk -> layout", func(t *testing.T) {
		layout, err := BootstrapDefaultLayout([]storage.BlockDevice{{Name: "nvme0n1"}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if layout.Mounts["/"].Device != "nvme0n1p2" {
			t.Errorf("root device = %q, want nvme0n1p2", layout.Mounts["/"].Device)
		}
	})

	t.Run("multi disk -> error names all disks", func(t *testing.T) {
		_, err := BootstrapDefaultLayout([]storage.BlockDevice{
			{Name: "nvme0n1"}, {Name: "nvme1n1"}, {Name: "nvme2n1"},
		})
		if err == nil {
			t.Fatal("expected refusal on multi-disk")
		}
		for _, want := range []string{"nvme0n1", "nvme1n1", "nvme2n1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error should name disk %q for operator triage, got: %v", want, err)
			}
		}
	})
}

// TestSummarizeLayout asserts the pre-partition summary names the real
// resolved root/ESP target (RAID resolves to /dev/mdN) and lists every
// command in order. This is the diagnostic that lets an operator catch
// a wrong-disk install before any disk is written.
func TestSummarizeLayout(t *testing.T) {
	// A RAID1 root layout like the supermicro kworker script produces.
	layout := storage.Layout{
		Devices: map[string]storage.Device{
			"nvme4n1": {
				Type: "disk", PTable: "gpt", Boot: true,
				Partitions: []storage.Partition{
					{Name: "nvme4n1p1", FS: "vfat", Size: "512M", Bootable: true},
					{Name: "nvme4n1p2", Size: "100G"},
				},
			},
			"nvme5n1": {
				Type: "disk", PTable: "gpt", Boot: true,
				Partitions: []storage.Partition{
					{Name: "nvme5n1p1", FS: "vfat", Size: "512M", Bootable: true},
					{Name: "nvme5n1p2", Size: "100G"},
				},
			},
			"mdroot": {
				Type: "raid", Level: 1,
				Members: []string{"nvme4n1p2", "nvme5n1p2"}, FS: "xfs",
			},
		},
		Mounts: map[string]storage.Mount{
			"/":         {Device: "mdroot"},
			"/boot/efi": {Device: "nvme4n1p1"},
		},
	}
	cmds, err := storage.Plan(layout, []storage.BlockDevice{
		{Name: "nvme4n1", SizeBytes: 1 << 40},
		{Name: "nvme5n1", SizeBytes: 1 << 40},
	})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	lines := SummarizeLayout(layout, cmds)
	joined := strings.Join(lines, "\n")

	// Root must resolve to the md device, NOT to "mdroot" the name.
	if !strings.Contains(joined, "mount / -> /dev/md0") {
		t.Errorf("summary should resolve root to /dev/md0, got:\n%s", joined)
	}
	if !strings.Contains(joined, "mount /boot/efi -> /dev/nvme4n1p1") {
		t.Errorf("summary should resolve ESP to /dev/nvme4n1p1, got:\n%s", joined)
	}
	// Every planned command must appear.
	if !strings.Contains(joined, fmt.Sprintf("%d command(s)", len(cmds))) {
		t.Errorf("summary should report command count %d, got:\n%s", len(cmds), joined)
	}
	if !strings.Contains(joined, "mdadm") {
		t.Errorf("summary should include the mdadm create command, got:\n%s", joined)
	}
}
