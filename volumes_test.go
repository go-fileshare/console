// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/go-widgets/mvvm"

	v1 "github.com/go-fileshare/console/internal/adminpb"

	"github.com/go-fileshare/console/internal/admin"
	"github.com/go-fileshare/console/internal/admin/admintest"
)

// volFixture is a server with a provisioner: a ZFS parent that snapshots,
// an XFS one that does not, and a 10 TiB cap.
func volFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t}
	f.a = admintest.NewServer("a", "Paris").WithVolumes(10<<40,
		&v1.Parent{Id: "tank", Kind: v1.VolumeKind_VOLUME_KIND_ZFS, Root: "/srv/tank", Snapshots: true, FreeBytes: 4 << 40, TotalBytes: 8 << 40},
		&v1.Parent{Id: "plain", Kind: v1.VolumeKind_VOLUME_KIND_XFS, Root: "/srv/plain"})
	f.fp = admintest.New(true, f.a)
	t.Cleanup(f.fp.Close)
	f.q = mvvm.NewQueue(nil)
	f.vm = newViewModel(context.Background(), admin.NewPortal(f.fp.URL, nil, nil), f.q)
	f.vm.Start()
	f.settle()
	return f
}

func TestVolumesLifecycle(t *testing.T) {
	f := volFixture(t)
	vm, vol := f.vm, f.vm.Vol
	if vol.Note.Get() != "" || vol.Parents.Len() != 2 || vol.Volumes.Len() != 0 {
		t.Fatalf("note %q, %d parents", vol.Note.Get(), vol.Parents.Len())
	}
	if parentLine(vol.Parents.At(0)) != "tank (ZFS dataset, 4.0 TiB free)" || parentLine(vol.Parents.At(1)) != "plain (XFS project)" {
		t.Fatal(parentLine(vol.Parents.At(0)), parentLine(vol.Parents.At(1)))
	}

	// Create on tank.
	vol.NewName.Set("lab")
	vol.NewSize.Set("2T")
	if !vol.Create.CanExecute() {
		t.Fatal("Create with a full form")
	}
	vol.Create.Execute()
	f.settle()
	if got := f.a.VolumeNames(); !slices.Equal(got, []string{"tank/lab"}) {
		t.Fatal(got)
	}
	v, _ := vm.volume()
	if v.GetQuotaBytes() != 2<<40 || volumeLine(v) != "tank/lab  2.0 TiB" || !slices.Contains(vol.Details.Slice(), "not shared") {
		t.Fatalf("%v %q %v", v, volumeLine(v), vol.Details.Slice())
	}
	// The same again: it exists, and says so.
	vol.NewName.Set("lab")
	vol.Create.Execute()
	f.settle()
	if !strings.Contains(vm.Status.Get(), "exists already") {
		t.Fatal(vm.Status.Get())
	}

	// Resize, refused below what is used.
	f.a.Use("tank", "lab", 1<<40)
	vm.Reload.Execute()
	f.settle()
	vol.Resize.Set("500G")
	calls := f.a.Calls.Load()
	vol.DoResize.Execute()
	f.settle()
	if f.a.Calls.Load() != calls || !strings.Contains(vm.Status.Get(), "in use") {
		t.Fatalf("a resize below use reached the server: %q", vm.Status.Get())
	}
	vol.Resize.Set("3T")
	vol.DoResize.Execute()
	f.settle()
	if v, _ := vm.volume(); v.GetQuotaBytes() != 3<<40 || !slices.Contains(vol.Details.Slice(), "used: 1.0 TiB (33%)") {
		t.Fatalf("%v %v", v, vol.Details.Slice())
	}

	// Snapshot.
	vol.SnapName.Set("before-upgrade")
	vol.Snapshot.Execute()
	f.settle()
	if v, _ := vm.volume(); !slices.Equal(v.GetSnapshots(), []string{"before-upgrade"}) {
		t.Fatal(v)
	}

	// Share it: the share form takes the volume as its source.
	vol.Share.Execute()
	if vm.Tab.Get() != tabShares || vm.NewDir.Get() != "volume:tank/lab" || vm.NewName.Get() != "lab" {
		t.Fatalf("tab %d, form %q %q", vm.Tab.Get(), vm.NewName.Get(), vm.NewDir.Get())
	}
	vm.NewSubject.Set("@lab")
	vm.Create.Execute()
	f.settle()
	if s, _ := vm.share(); s.GetVolume().GetName() != "lab" || !strings.Contains(strings.Join(shareDetails(s), "|"), "volume: volume tank/lab") {
		t.Fatalf("%v", s)
	}

	// A shared volume is not deleted: delete the share first.
	vm.Tab.Set(tabVolumes)
	vm.Reload.Execute()
	f.settle()
	calls = f.a.Calls.Load()
	vol.Delete.Execute()
	f.settle()
	if f.a.Calls.Load() != calls || !strings.Contains(vm.Status.Get(), "delete the share first") {
		t.Fatalf("%q", vm.Status.Get())
	}
	vm.Delete.Execute() // the share: armed
	vm.Delete.Execute() // deleted
	f.settle()
	// Two presses, and the server refuses a volume holding data: the console
	// never asks it to destroy any.
	vol.Delete.Execute()
	f.settle()
	if !strings.Contains(vm.Status.Get(), "Press Delete again") {
		t.Fatal(vm.Status.Get())
	}
	vol.Delete.Execute()
	f.settle()
	if !strings.Contains(vm.Status.Get(), "holds data or snapshots") || len(f.a.VolumeNames()) != 1 || f.a.Destroyed {
		t.Fatalf("%q, %v, destroyed %v", vm.Status.Get(), f.a.VolumeNames(), f.a.Destroyed)
	}
}

// An empty volume on a parent that cannot snapshot: Snapshot is greyed,
// and Delete deletes after two presses.
func TestVolumeWithoutSnapshotsAndDelete(t *testing.T) {
	f := volFixture(t)
	vm, vol := f.vm, f.vm.Vol
	vol.NewParent.Set(1)
	vol.NewName.Set("scratch")
	vol.NewSize.Set("100G")
	vol.Create.Execute()
	f.settle()
	vol.SnapName.Set("x")
	if vol.Snapshot.CanExecute() || !slices.Contains(vol.Details.Slice(), "snapshots: not on XFS project") {
		t.Fatalf("snapshots offered on XFS: %v", vol.Details.Slice())
	}
	vol.Delete.Execute()
	vol.Delete.Execute()
	f.settle()
	if len(f.a.VolumeNames()) != 0 || vol.Volumes.Len() != 0 {
		t.Fatal(f.a.VolumeNames())
	}
	_ = vm
}

// A server without a provisioner shows why, and offers nothing to press.
func TestNoProvisioner(t *testing.T) {
	f := newFixture(t) // its servers have no provisioner
	vol := f.vm.Vol
	if vol.Note.Get() != "This server manages no volumes." || vol.Create.CanExecute() ||
		!strings.Contains(strings.Join(vol.Details.Slice(), "|"), "why: no provisioner block") {
		t.Fatalf("note %q, details %v", vol.Note.Get(), vol.Details.Slice())
	}
	if f.vm.Status.Get() != "" {
		t.Fatalf("no provisioner shown as a failure: %q", f.vm.Status.Get())
	}
}

func TestVolumeFormRefusals(t *testing.T) {
	f := volFixture(t)
	vm, vol := f.vm, f.vm.Vol
	calls := f.a.Calls.Load()
	for _, c := range []struct{ name, size, want string }{
		{"Lab", "1T", "a name is"},
		{"lab", "lots", "not a size"},
		{"lab", "0", "a size of 0"},
		{"lab", "20T", "at most 10.0 TiB"},
	} {
		vol.NewName.Set(c.name)
		vol.NewSize.Set(c.size)
		vol.Create.Execute()
		f.settle()
		if !strings.Contains(vm.Status.Get(), c.want) {
			t.Errorf("%s %s: %q", c.name, c.size, vm.Status.Get())
		}
	}
	if f.a.Calls.Load() != calls {
		t.Fatal("a refused form reached the server")
	}
	// A share from a malformed volume reference.
	vm.NewName.Set("x")
	vm.NewDir.Set("volume:tank")
	vm.NewSubject.Set("alice")
	vm.Create.Execute()
	f.settle()
	if !strings.Contains(vm.Status.Get(), "volume:<parent>/<name>") {
		t.Fatal(vm.Status.Get())
	}
	// Resize and snapshot names, with a volume selected.
	vol.NewName.Set("v")
	vol.NewSize.Set("1G")
	vol.Create.Execute()
	f.settle()
	vol.Resize.Set("big")
	vol.DoResize.Execute()
	vol.SnapName.Set("Bad Name")
	vol.Snapshot.Execute()
	if !strings.HasPrefix(vm.Status.Get(), "Snapshot: a name is") {
		t.Fatal(vm.Status.Get())
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"1024": 1024, "500M": 500 << 20, "10G": 10 << 30, "1.5T": 3 << 39,
		"2 TiB": 2 << 40, "1p": 1 << 50, "4k": 4096, " 3gb ": 3 << 30} {
		if got, err := parseSize(in); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "-1G", "0", "0.1", "1Q", "99999999999999999999P"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
