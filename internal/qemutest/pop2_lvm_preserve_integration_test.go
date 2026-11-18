//go:build integration

package qemutest_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bluesky-social/slagworks/internal/model"
	"github.com/bluesky-social/slagworks/internal/qemutest"
)

// TestDeployPop2KworkerPreservesOpenEBSLVM exercises the shipped pop2
// recovery asset against a scaled copy of the real disk topology: two boot
// NVMes and eight pre-existing data NVMes. The data qcow2s contain an
// openebs-vg with one LV pinned to each PV. UUIDs and marker bytes are
// captured before PXE deploy and compared after the VM has installed and
// booted from its newly-created NVMe RAID1.
//
// WipeAllDisks is deliberately enabled. This is the strongest path: every
// unclaimed disk would be wiped, so survival proves all eight retain entries
// reached both the planner and bystander-wipe exclusion in the real deploy
// flow.
func TestDeployPop2KworkerPreservesOpenEBSLVM(t *testing.T) {
	t.Parallel()
	lockPop2LVMTest(t)

	env := qemutest.New(t)
	network := env.NewNetwork(t)
	images := env.UseCanonicalImages(t)

	diskDir, err := os.MkdirTemp("/tmp", "slagworks-qemutest-pop2-lvm-")
	if err != nil {
		t.Fatalf("create pop2 data-disk scratch dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(diskDir) })
	dataPaths := make([]string, 8)
	for i := range dataPaths {
		dataPaths[i] = filepath.Join(diskDir, fmt.Sprintf("data-%d.qcow2", i))
		env.CreateDisk(t, dataPaths[i], "512M")
	}

	seeded := attachNBDDisks(t, dataPaths)
	wantIdentity := seeded.seedOpenEBSVG()
	wantHashes := hashBlockDevices(t, seeded.devices)
	seeded.disconnect()

	mac := "52:54:00:34:00:08"
	systemUUID := "7bf9c4a8-2686-4f08-98f2-000000340008"
	machineID := uuid.New()
	storageScript := readAssetScript(t, "storage", "250-pop2-kworker-preserve-lvm")

	deployOp := qemutest.NewOperation(machineID, model.OpDeploy)
	deployOp.Params = mustMarshalParams(t, model.OpParams{
		Destructive:  true,
		WipeAllDisks: true,
	})
	machine := qemutest.Machine{
		ID:          machineID,
		MAC:         mac,
		State:       model.MachineStateDeploying,
		PXEEligible: true,
		SystemUUID:  systemUUID,
		StorageScript: &model.SnapshotStorageScript{
			Name:    "250-pop2-kworker-preserve-lvm",
			Content: storageScript,
		},
	}

	const slug = "pop2-lvm-preserve"
	snap := env.NewSnapshot(slug, network).
		AddMachine(machine).
		AddOperation(deployOp).
		Build()
	env.Hub.Push(slug, snap)
	env.NewSiteWorker(t, qemutest.SiteWorkerConfig{
		Slug:         slug,
		Network:      network,
		Images:       images,
		DefaultImage: "noble-pxe",
	})

	disks := []qemutest.ExtraDisk{
		{Type: qemutest.DiskNVMe, Size: "6G", Serial: "MZ1L23T8HBLA-QEMU-0"},
		{Type: qemutest.DiskNVMe, Size: "6G", Serial: "MZ1L23T8HBLA-QEMU-1"},
	}
	for i, path := range dataPaths {
		disks = append(disks, qemutest.ExtraDisk{
			Type:   qemutest.DiskNVMe,
			Path:   path,
			Serial: fmt.Sprintf("MZQL23T8HCLS-QEMU-%d", i),
		})
	}

	vm := env.NewVM(t, qemutest.VMConfig{
		Firmware:           qemutest.UEFI,
		MAC:                mac,
		Network:            network,
		Memory:             "4096",
		CPUs:               "4",
		NoPrimaryDisk:      true,
		ExtraDisks:         disks,
		SMBIOSUUID:         systemUUID,
		SMBIOSManufacturer: "QEMU",
		SMBIOSProduct:      "AS -1115HS-TNR",
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	vm.Start(ctx)

	if err := vm.Serial.WaitFor(ctx, `"msg":"deploy complete"`, qemutest.Budgets.DeployComplete); err != nil {
		t.Fatalf("waiting for pop2 deploy complete: %v\n%s", err, serialDump(vm))
	}
	awaiting := deployOp
	awaiting.Status = model.OpClaimed
	awaiting.Phase = "awaiting_phone_home"
	awaitingSnap := env.NewSnapshot(slug, network).
		Seq(2).
		AddMachine(machine).
		AddOperation(awaiting).
		Build()
	env.Hub.Push(slug, awaitingSnap)
	if _, err := env.Hub.WaitForObservation(ctx, slug, "deploy_complete", machineID); err != nil {
		t.Fatalf("waiting for deploy_complete observation: %v", err)
	}
	if _, err := env.Hub.WaitForObservation(ctx, slug, "phone_home", machineID); err != nil {
		t.Fatalf("new pop2 root did not boot and phone home: %v\n%s", err, serialDump(vm))
	}
	vm.Stop()

	inspected := attachNBDDisks(t, dataPaths)
	// Hash raw guest-visible bytes through NBD, not the qcow2 containers:
	// QEMU may update qcow2 bookkeeping on a writable attach even when the
	// guest does not write. This catches changes anywhere on a retained disk,
	// including outside allocated LVM extents, without that false-positive.
	if gotHashes := hashBlockDevices(t, inspected.devices); !reflect.DeepEqual(gotHashes, wantHashes) {
		t.Errorf("one or more retained data disks changed during deploy\nwant %v\n got %v", wantHashes, gotHashes)
	}
	gotIdentity := inspected.inspectOpenEBSVG()
	inspected.disconnect()
	if !reflect.DeepEqual(gotIdentity, wantIdentity) {
		t.Fatalf("openebs-vg identity changed across pop2 deploy\nwant: %+v\n got: %+v", wantIdentity, gotIdentity)
	}
}

type openEBSIdentity struct {
	VGUUID  string
	PVUUIDs []string
	LVUUIDs map[string]string
	Markers map[string]string
}

type nbdDisks struct {
	t            *testing.T
	devices      []string
	lvmEnv       []string
	disconnected bool
}

func lockPop2LVMTest(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile("/tmp/slagworks-pop2-lvm-qemu.lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatalf("open NBD/LVM test lock: %v", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		t.Fatalf("lock NBD/LVM test: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	})
}

func attachNBDDisks(t *testing.T, images []string) *nbdDisks {
	t.Helper()
	runHost(t, nil, "modprobe", "nbd", "max_part=8")

	available, err := filepath.Glob("/sys/class/block/nbd*")
	if err != nil {
		t.Fatalf("list NBD devices: %v", err)
	}
	sort.Slice(available, func(i, j int) bool { return nbdNumber(available[i]) < nbdNumber(available[j]) })

	n := &nbdDisks{t: t}
	for _, sysPath := range available {
		if len(n.devices) == len(images) {
			break
		}
		pid, err := os.ReadFile(filepath.Join(sysPath, "pid"))
		if err == nil && strings.TrimSpace(string(pid)) != "" && strings.TrimSpace(string(pid)) != "0" {
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatalf("inspect %s/pid: %v", sysPath, err)
		}
		dev := "/dev/" + filepath.Base(sysPath)
		image := images[len(n.devices)]
		runHost(t, nil, "qemu-nbd", "--format=qcow2", "--connect="+dev, image)
		waitBlockDevice(t, dev)
		n.devices = append(n.devices, dev)
	}
	if len(n.devices) != len(images) {
		n.disconnect()
		t.Fatalf("need %d free NBD devices, found %d", len(images), len(n.devices))
	}

	lvmDir := t.TempDir()
	filters := make([]string, 0, len(n.devices)+1)
	for _, dev := range n.devices {
		filters = append(filters, fmt.Sprintf(`"a|^%s$|"`, dev))
	}
	filters = append(filters, `"r|.*|"`)
	config := fmt.Sprintf(`devices {
    obtain_device_list_from_udev = 0
    use_devicesfile = 0
    filter = [ %s ]
    global_filter = [ %s ]
}
activation {
    udev_sync = 0
    udev_rules = 0
}
`, strings.Join(filters, ", "), strings.Join(filters, ", "))
	if err := os.WriteFile(filepath.Join(lvmDir, "lvm.conf"), []byte(config), 0600); err != nil {
		n.disconnect()
		t.Fatalf("write isolated lvm.conf: %v", err)
	}
	n.lvmEnv = append(os.Environ(), "LVM_SYSTEM_DIR="+lvmDir)
	t.Cleanup(n.disconnect)
	return n
}

func (n *nbdDisks) seedOpenEBSVG() openEBSIdentity {
	n.t.Helper()
	args := append([]string{"-ff", "-y"}, n.devices...)
	n.lvm("pvcreate", args...)
	vgArgs := append([]string{"--addtag", "slagworks_qemutest", "openebs-vg"}, n.devices...)
	n.lvm("vgcreate", vgArgs...)
	for i, dev := range n.devices {
		name := fmt.Sprintf("retained%d", i)
		n.lvm("lvcreate", "-y", "-L", "16M", "-n", name, "openebs-vg", dev)
		marker := fmt.Sprintf("pop2-retained-pv-%d", i)
		lvPath := filepath.Join("/dev/openebs-vg", name)
		f, err := os.OpenFile(lvPath, os.O_WRONLY, 0)
		if err != nil {
			n.t.Fatalf("open %s for marker: %v", lvPath, err)
		}
		if _, err := f.WriteAt([]byte(marker), 4096); err != nil {
			_ = f.Close()
			n.t.Fatalf("write marker to %s: %v", lvPath, err)
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			n.t.Fatalf("sync marker on %s: %v", lvPath, err)
		}
		_ = f.Close()
	}
	runHost(n.t, nil, "sync")
	return n.readIdentity()
}

func (n *nbdDisks) inspectOpenEBSVG() openEBSIdentity {
	n.t.Helper()
	n.lvm("vgchange", "-ay", "openebs-vg")
	return n.readIdentity()
}

func (n *nbdDisks) readIdentity() openEBSIdentity {
	n.t.Helper()
	id := openEBSIdentity{
		LVUUIDs: make(map[string]string),
		Markers: make(map[string]string),
	}
	id.VGUUID = strings.TrimSpace(n.lvmOutput("vgs", "--noheadings", "-o", "vg_uuid", "openebs-vg"))
	for _, line := range strings.Split(n.lvmOutput("pvs", "--noheadings", "--separator", "|", "-o", "pv_name,pv_uuid,vg_name"), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 3 || strings.TrimSpace(fields[2]) != "openebs-vg" {
			continue
		}
		id.PVUUIDs = append(id.PVUUIDs, strings.TrimSpace(fields[1]))
	}
	sort.Strings(id.PVUUIDs)
	for _, line := range strings.Split(n.lvmOutput("lvs", "--noheadings", "--separator", "|", "-o", "lv_name,lv_uuid", "openebs-vg"), "\n") {
		fields := strings.Split(line, "|")
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		id.LVUUIDs[name] = strings.TrimSpace(fields[1])
		lvPath := filepath.Join("/dev/openebs-vg", name)
		f, err := os.Open(lvPath)
		if err != nil {
			n.t.Fatalf("open %s for marker read: %v", lvPath, err)
		}
		buf := make([]byte, 64)
		count, err := f.ReadAt(buf, 4096)
		_ = f.Close()
		if err != nil && count == 0 {
			n.t.Fatalf("read marker from %s: %v", lvPath, err)
		}
		marker := strings.TrimRight(string(buf[:count]), "\x00")
		wantMarker := "pop2-retained-pv-" + strings.TrimPrefix(name, "retained")
		if marker != wantMarker {
			n.t.Fatalf("marker on %s = %q, want %q", lvPath, marker, wantMarker)
		}
		id.Markers[name] = marker
	}
	return id
}

func (n *nbdDisks) lvm(name string, args ...string) {
	n.t.Helper()
	runHost(n.t, n.lvmEnv, name, args...)
}

func (n *nbdDisks) lvmOutput(name string, args ...string) string {
	n.t.Helper()
	return runHost(n.t, n.lvmEnv, name, args...)
}

func (n *nbdDisks) disconnect() {
	if n == nil || n.disconnected {
		return
	}
	n.disconnected = true
	if len(n.lvmEnv) > 0 {
		cmd := exec.Command("vgchange", "-an", "openebs-vg")
		cmd.Env = n.lvmEnv
		if out, err := cmd.CombinedOutput(); err != nil {
			n.t.Errorf("deactivate test openebs-vg: %v\n%s", err, out)
		}
	}
	for i := len(n.devices) - 1; i >= 0; i-- {
		if out, err := exec.Command("qemu-nbd", "--disconnect", n.devices[i]).CombinedOutput(); err != nil {
			n.t.Errorf("disconnect %s: %v\n%s", n.devices[i], err, out)
		}
	}
}

func nbdNumber(path string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "nbd"))
	if err != nil {
		return int(^uint(0) >> 1)
	}
	return n
}

func waitBlockDevice(t *testing.T, dev string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("blockdev", "--getsize64", dev)
		if out, err := cmd.Output(); err == nil && strings.TrimSpace(string(out)) != "0" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("NBD device %s never became ready", dev)
}

func runHost(t *testing.T, env []string, name string, args ...string) string {
	t.Helper()
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = env
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}
	return string(out)
}

func hashBlockDevices(t *testing.T, devices []string) map[string]string {
	t.Helper()
	hashes := make(map[string]string, len(devices))
	for i, dev := range devices {
		f, err := os.Open(dev)
		if err != nil {
			t.Fatalf("open %s for hash: %v", dev, err)
		}
		h := sha256.New()
		if _, err := io.Copy(h, f); err != nil {
			_ = f.Close()
			t.Fatalf("hash %s: %v", dev, err)
		}
		_ = f.Close()
		hashes[strconv.Itoa(i)] = hex.EncodeToString(h.Sum(nil))
	}
	return hashes
}
