// SPDX-License-Identifier: BSD-3-Clause
//
// Volumes: the storage a server's provisioner makes -- a ZFS dataset, a
// btrfs subvolume, an XFS or ext4 directory under a project quota -- sized,
// and shared like a directory. ViewModel state only, as viewmodel.go.

package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/go-widgets/mvvm"

	v1 "github.com/go-fileshare/console/internal/adminpb"
)

// volumeVM is the Volumes tab.
type volumeVM struct {
	// Note says why there are no volumes to manage, when there are none
	// to: no provisioner on this server, or no server reachable.
	Note    *mvvm.Observable[string]
	Parents *mvvm.ObservableList[*v1.Parent]
	Volumes *mvvm.ObservableList[*v1.Volume]
	VolSel  *mvvm.Observable[int]
	Details *mvvm.ObservableList[string]

	// The new-volume form, a resize, a snapshot name.
	NewName   *mvvm.Observable[string]
	NewParent *mvvm.Observable[int]
	NewSize   *mvvm.Observable[string]
	Resize    *mvvm.Observable[string]
	SnapName  *mvvm.Observable[string]

	Create, DoResize, Snapshot, Delete, Share *mvvm.Command

	maxBytes uint64
	armed    string // the volume a first Delete armed
}

func newVolumeVM() *volumeVM {
	return &volumeVM{
		Note: mvvm.NewObservable(""), Parents: mvvm.NewObservableList[*v1.Parent](),
		Volumes: mvvm.NewObservableList[*v1.Volume](), VolSel: mvvm.NewObservable(-1),
		Details: mvvm.NewObservableList[string](),
		NewName: mvvm.NewObservable(""), NewParent: mvvm.NewObservable(0), NewSize: mvvm.NewObservable(""),
		Resize: mvvm.NewObservable(""), SnapName: mvvm.NewObservable(""),
	}
}

// wireVolumes builds the volume commands, once vm exists.
func (vm *viewModel) wireVolumes() {
	v := vm.Vol
	has := func() bool { _, ok := vm.volume(); return ok }
	v.Create = mvvm.NewCommand(vm.createVolume, func() bool {
		_, ok := vm.server()
		return ok && v.Parents.Len() > 0 && strings.TrimSpace(v.NewName.Get()) != "" && strings.TrimSpace(v.NewSize.Get()) != ""
	})
	v.DoResize = mvvm.NewCommand(vm.resizeVolume, func() bool { return has() && strings.TrimSpace(v.Resize.Get()) != "" })
	v.Snapshot = mvvm.NewCommand(vm.snapshotVolume, func() bool {
		vol, ok := vm.volume()
		return ok && strings.TrimSpace(v.SnapName.Get()) != "" && vm.parentOf(vol).GetSnapshots()
	})
	v.Delete = mvvm.NewCommand(vm.deleteVolume, has)
	v.Share = mvvm.NewCommand(vm.shareVolume, has)
	mvvm.BindCanExecute(v.Create, vm.ServerSel, vm.Servers, v.Parents, v.NewName, v.NewSize)
	mvvm.BindCanExecute(v.DoResize, v.VolSel, v.Volumes, v.Resize)
	mvvm.BindCanExecute(v.Snapshot, v.VolSel, v.Volumes, v.SnapName)
	mvvm.BindCanExecute(v.Delete, v.VolSel, v.Volumes)
	mvvm.BindCanExecute(v.Share, v.VolSel, v.Volumes)
	v.VolSel.SubscribeChanged(vm.showVolume)
}

func (vm *viewModel) volume() (*v1.Volume, bool) {
	v := vm.Vol
	i := v.VolSel.Get()
	if i < 0 || i >= v.Volumes.Len() {
		return nil, false
	}
	return v.Volumes.At(i), true
}

func (vm *viewModel) parentOf(vol *v1.Volume) *v1.Parent {
	for _, p := range vm.Vol.Parents.Slice() {
		if p.GetId() == vol.GetParent() {
			return p
		}
	}
	return nil
}

type volumeState struct {
	parents *v1.ListParentsResponse
	volumes []*v1.Volume
	none    string // why there are none to manage, when there are not
}

// noVolumes says whether err is a server that manages no volumes -- no
// provisioner, or one that refuses this server -- which is a state to show,
// not a failure.
func noVolumes(err error) (string, bool) {
	var ce *connect.Error
	if errors.As(err, &ce) && (ce.Code() == connect.CodeFailedPrecondition || ce.Code() == connect.CodeUnimplemented) {
		return ce.Message(), true
	}
	return "", false
}

// loadVolumes reads the selected server's parents and volumes, keeping the
// volume named keep (parent/name) selected.
func (vm *viewModel) loadVolumes(keep string) {
	s, ok := vm.server()
	v := vm.Vol
	v.armed = ""
	if !ok {
		v.Note.Set("")
		vm.setVolumes(nil, nil, "")
		return
	}
	c := vm.be.Client(s.ID)
	async(vm, s.Name()+" volumes", func(ctx context.Context) (volumeState, error) {
		ps, err := c.ListParents(ctx, &v1.ListParentsRequest{})
		if err != nil {
			if why, ok := noVolumes(err); ok {
				return volumeState{none: why}, nil
			}
			return volumeState{}, err
		}
		vs, err := c.ListVolumes(ctx, &v1.ListVolumesRequest{})
		if err != nil {
			return volumeState{}, err
		}
		return volumeState{parents: ps, volumes: vs.GetVolumes()}, nil
	}, func(st volumeState) {
		v.maxBytes = st.parents.GetMaxVolumeBytes()
		vm.setVolumes(st.parents.GetParents(), st.volumes, keep)
		v.Note.Set("")
		if st.none != "" {
			// A short note where it is read first, the server's reason in
			// the details, which have the room.
			v.Note.Set("This server manages no volumes.")
			v.Details.Clear()
			v.Details.Append("why: "+st.none, "volumes need the fileshare provisioner: see docs/volumes.md")
		}
	})
}

// reloadVolumes reads the volumes again, keeping the selected one: which
// shares use a volume changes with the shares.
func (vm *viewModel) reloadVolumes() {
	keep := ""
	if vol, ok := vm.volume(); ok {
		keep = volumeKey(vol)
	}
	vm.loadVolumes(keep)
}

func volumeKey(vol *v1.Volume) string { return vol.GetParent() + "/" + vol.GetName() }

func (vm *viewModel) setVolumes(parents []*v1.Parent, vols []*v1.Volume, keep string) {
	v := vm.Vol
	v.Parents.Clear()
	v.Parents.Append(parents...)
	if v.NewParent.Get() >= len(parents) {
		v.NewParent.Set(0)
	}
	v.Volumes.Clear()
	v.Volumes.Append(vols...)
	sel := slices.IndexFunc(vols, func(x *v1.Volume) bool { return volumeKey(x) == keep })
	if sel < 0 && len(vols) > 0 {
		sel = 0
	}
	if v.VolSel.Get() == sel {
		vm.showVolume()
	}
	v.VolSel.Set(sel)
}

func (vm *viewModel) showVolume() {
	v := vm.Vol
	v.armed = ""
	v.Details.Clear()
	vol, ok := vm.volume()
	if !ok {
		return
	}
	v.Details.Append(volumeDetails(vol, vm.parentOf(vol))...)
}

func (vm *viewModel) createVolume() {
	s, _ := vm.server()
	v := vm.Vol
	name := strings.TrimSpace(v.NewName.Get())
	if !volumeName.MatchString(name) {
		vm.Status.Set("New volume: a name is 1 to 63 of a-z, 0-9, _ and -, starting with a letter or a digit")
		return
	}
	size, err := parseSize(v.NewSize.Get())
	if err != nil {
		vm.Status.Set("New volume: " + err.Error())
		return
	}
	if v.maxBytes > 0 && size > v.maxBytes {
		vm.Status.Set("New volume: at most " + bytesString(v.maxBytes) + " on this server")
		return
	}
	i := v.NewParent.Get()
	if i < 0 || i >= v.Parents.Len() {
		i = 0
	}
	parent := v.Parents.At(i).GetId()
	c := vm.be.Client(s.ID)
	async(vm, "Create volume "+name, func(ctx context.Context) (*v1.CreateVolumeResponse, error) {
		return c.CreateVolume(ctx, &v1.CreateVolumeRequest{Parent: parent, Name: name, QuotaBytes: size})
	}, func(r *v1.CreateVolumeResponse) {
		v.NewName.Set("")
		if r.GetCreated() {
			vm.Status.Set("Volume " + parent + "/" + name + " created, " + bytesString(size) + ".")
		} else {
			vm.Status.Set("Volume " + parent + "/" + name + " exists already, with that size.")
		}
		vm.loadVolumes(parent + "/" + name)
	})
}

func (vm *viewModel) resizeVolume() {
	s, _ := vm.server()
	v := vm.Vol
	vol, _ := vm.volume()
	size, err := parseSize(v.Resize.Get())
	if err != nil {
		vm.Status.Set("Resize: " + err.Error())
		return
	}
	if u := vol.GetUsedBytes(); u > 0 && size < u {
		vm.Status.Set("Resize: " + bytesString(u) + " are in use, more than " + bytesString(size))
		return
	}
	c := vm.be.Client(s.ID)
	key := volumeKey(vol)
	async(vm, "Resize "+key, func(ctx context.Context) (*v1.ResizeVolumeResponse, error) {
		return c.ResizeVolume(ctx, &v1.ResizeVolumeRequest{Parent: vol.GetParent(), Name: vol.GetName(), QuotaBytes: size})
	}, func(*v1.ResizeVolumeResponse) {
		v.Resize.Set("")
		vm.Status.Set("Volume " + key + " is now " + bytesString(size) + ".")
		vm.loadVolumes(key)
	})
}

func (vm *viewModel) snapshotVolume() {
	s, _ := vm.server()
	v := vm.Vol
	vol, _ := vm.volume()
	snap := strings.TrimSpace(v.SnapName.Get())
	if !volumeName.MatchString(snap) {
		vm.Status.Set("Snapshot: a name is 1 to 63 of a-z, 0-9, _ and -, starting with a letter or a digit")
		return
	}
	c := vm.be.Client(s.ID)
	key := volumeKey(vol)
	async(vm, "Snapshot "+key, func(ctx context.Context) (*v1.SnapshotVolumeResponse, error) {
		return c.SnapshotVolume(ctx, &v1.SnapshotVolumeRequest{Parent: vol.GetParent(), Name: vol.GetName(), Snapshot: snap})
	}, func(r *v1.SnapshotVolumeResponse) {
		v.SnapName.Set("")
		if r.GetCreated() {
			vm.Status.Set("Snapshot " + key + "@" + snap + " taken.")
		} else {
			vm.Status.Set("Snapshot " + key + "@" + snap + " existed already.")
		}
		vm.loadVolumes(key)
	})
}

// deleteVolume is two presses, and never destroys data: a volume that holds
// some, or snapshots, is refused by the server, and the console says why.
// Destroying data is not something to do from a list with a button.
func (vm *viewModel) deleteVolume() {
	s, _ := vm.server()
	v := vm.Vol
	vol, _ := vm.volume()
	key := volumeKey(vol)
	if n := len(vol.GetShares()); n > 0 {
		vm.Status.Set("Volume " + key + " is used by " + strings.Join(vol.GetShares(), ", ") + ": delete the share first.")
		return
	}
	if v.armed != key {
		v.armed = key
		vm.Status.Set("Press Delete again to delete volume " + key + ". One that holds data or snapshots is refused.")
		return
	}
	v.armed = ""
	c := vm.be.Client(s.ID)
	async(vm, "Delete "+key, func(ctx context.Context) (*v1.DeleteVolumeResponse, error) {
		return c.DeleteVolume(ctx, &v1.DeleteVolumeRequest{Parent: vol.GetParent(), Name: vol.GetName()})
	}, func(*v1.DeleteVolumeResponse) {
		vm.Status.Set("Volume " + key + " deleted.")
		vm.loadVolumes("")
	})
}

// shareVolume starts a share of the selected volume: the new-share form,
// with the volume as its source.
func (vm *viewModel) shareVolume() {
	vol, _ := vm.volume()
	vm.NewDir.Set(volumePrefix + volumeKey(vol))
	if vm.NewName.Get() == "" {
		vm.NewName.Set(vol.GetName())
	}
	vm.Tab.Set(tabShares)
	vm.Status.Set("Say who may use it, and press Create.")
}

// volumePrefix marks a new share's source as a volume rather than a
// directory: volume:<parent>/<name>.
const volumePrefix = "volume:"

// shareSource reads the new-share form's source.
func shareSource(s string) (*v1.CreateShareRequest, error) {
	s = strings.TrimSpace(s)
	if rest, ok := strings.CutPrefix(s, volumePrefix); ok {
		parent, name, ok := strings.Cut(rest, "/")
		if !ok || parent == "" || name == "" {
			return nil, fmt.Errorf("%q: a volume is volume:<parent>/<name>", s)
		}
		return &v1.CreateShareRequest{Source: &v1.CreateShareRequest_Volume{Volume: &v1.VolumeRef{Parent: parent, Name: name}}}, nil
	}
	return &v1.CreateShareRequest{Source: &v1.CreateShareRequest_Directory{Directory: s}}, nil
}

// volumeName is the provisioner's grammar, for volumes and snapshots.
var volumeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

var sizeRE = regexp.MustCompile(`(?i)^\s*([0-9]+(?:\.[0-9]+)?)\s*([kmgtp]?)(?:i?b)?\s*$`)

// parseSize reads a quota: 500M, 10G, 1.5T -- binary units, as fileshare's
// own configuration reads them -- or bytes.
func parseSize(s string) (uint64, error) {
	m := sizeRE.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("%q is not a size: 500M, 10G, 1.5T", strings.TrimSpace(s))
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	shift := strings.Index("kmgtp", strings.ToLower(m[2])) + 1
	if m[2] == "" {
		shift = 0
	}
	b := n * math.Pow(1024, float64(shift))
	if b < 1 {
		return 0, errors.New("a size of 0 is refused: an unbounded volume is a configuration mistake")
	}
	if b > math.MaxUint64/2 {
		return 0, fmt.Errorf("%q is too large", strings.TrimSpace(s))
	}
	return uint64(b), nil
}

var kindNames = map[v1.VolumeKind]string{v1.VolumeKind_VOLUME_KIND_ZFS: "ZFS dataset",
	v1.VolumeKind_VOLUME_KIND_BTRFS: "btrfs subvolume", v1.VolumeKind_VOLUME_KIND_XFS: "XFS project",
	v1.VolumeKind_VOLUME_KIND_EXT4: "ext4 project"}

// parentLine is how a parent is offered.
func parentLine(p *v1.Parent) string {
	line := p.GetId() + " (" + kindNames[p.GetKind()]
	if p.GetTotalBytes() > 0 {
		line += ", " + bytesString(p.GetFreeBytes()) + " free"
	}
	return line + ")"
}

// volumeLine is how a volume is listed.
func volumeLine(vol *v1.Volume) string {
	line := volumeKey(vol) + "  " + bytesString(vol.GetQuotaBytes())
	if n := len(vol.GetShares()); n > 0 {
		line += "  shared"
	}
	return line
}

func volumeDetails(vol *v1.Volume, p *v1.Parent) []string {
	out := []string{kindNames[vol.GetKind()] + ": " + vol.GetPath(), "quota: " + bytesString(vol.GetQuotaBytes())}
	if u := vol.GetUsedBytes(); u > 0 && vol.GetQuotaBytes() > 0 {
		out = append(out, fmt.Sprintf("used: %s (%.0f%%)", bytesString(u), 100*float64(u)/float64(vol.GetQuotaBytes())))
	}
	if c := vol.GetCreated(); c != nil {
		out = append(out, "created: "+c.AsTime().UTC().Format("2006-01-02 15:04 UTC"))
	}
	if s := vol.GetShares(); len(s) > 0 {
		out = append(out, "shared as: "+strings.Join(s, ", "))
	} else {
		out = append(out, "not shared")
	}
	switch {
	case len(vol.GetSnapshots()) > 0:
		out = append(out, "snapshots: "+strings.Join(vol.GetSnapshots(), ", "))
	case p != nil && !p.GetSnapshots():
		out = append(out, "snapshots: not on "+kindNames[p.GetKind()])
	}
	return out
}
