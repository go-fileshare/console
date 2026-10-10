// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"github.com/go-widgets/mvvm"

	v1 "github.com/go-fileshare/console/internal/adminpb"

	"github.com/go-fileshare/console/internal/admin"
	"github.com/go-fileshare/console/internal/admin/admintest"
)

// fixture is the ViewModel over the real portal client and a fake portal
// with two servers, A reachable with two shares and B behind another login.
type fixture struct {
	t     *testing.T
	fp    *admintest.Portal
	a     *admintest.Server
	vm    *viewModel
	q     *mvvm.Queue
	wakes atomic.Int64 // wake runs on the posting goroutines
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t}
	f.a = admintest.NewServer("a", "Paris",
		admintest.Share("photos", "/srv/photos", "alice", true),
		admintest.Share("archive", "/srv/archive", "@staff", false))
	b := admintest.NewServer("b", "")
	b.Reachable = false
	f.fp = admintest.New(true, f.a, b)
	t.Cleanup(f.fp.Close)
	f.q = mvvm.NewQueue(func() { f.wakes.Add(1) })
	f.vm = newViewModel(context.Background(), admin.NewPortal(f.fp.URL, nil, nil), f.q)
	f.vm.Start()
	f.settle()
	return f
}

// settle plays the UI goroutine: it drains the queue until no call is in
// flight. Every result reaches the ViewModel here, never elsewhere.
func (f *fixture) settle() {
	f.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for f.vm.pending > 0 || f.q.Len() > 0 {
		f.q.Drain()
		if time.Now().After(deadline) {
			f.t.Fatal("calls still in flight after 10 s")
		}
		time.Sleep(time.Millisecond)
	}
}

func (f *fixture) shareNames() []string {
	var out []string
	for _, s := range f.vm.Shares.Slice() {
		out = append(out, s.GetName())
	}
	return out
}

func TestStartListsServersAndShares(t *testing.T) {
	f := newFixture(t)
	vm := f.vm
	if vm.User.Get() != "alice" || vm.Servers.Len() != 2 || vm.ServerSel.Get() != 0 {
		t.Fatalf("user %q, %d servers, selected %d", vm.User.Get(), vm.Servers.Len(), vm.ServerSel.Get())
	}
	if got := strings.Join(f.shareNames(), ","); got != "archive,photos" {
		t.Fatal(got)
	}
	if !strings.HasPrefix(vm.ServerInfo.Get(), "A - fileshare v0.28.1, 2 shares") {
		t.Fatalf("info %q", vm.ServerInfo.Get())
	}
	// The first share is selected, its details and grants derived.
	if vm.ShareSel.Get() != 0 || vm.Grants.Len() != 1 || grantLine(vm.Grants.At(0)) != "@staff (read)" {
		t.Fatalf("selection %d, grants %v", vm.ShareSel.Get(), vm.Grants.Slice())
	}
	if !slices.Contains(vm.Details.Slice(), "directory: /srv/archive") {
		t.Fatal(vm.Details.Slice())
	}
	if vm.Busy.Get() || vm.Status.Get() != "" || f.wakes.Load() == 0 {
		t.Fatalf("busy %v, status %q, %d wakes", vm.Busy.Get(), vm.Status.Get(), f.wakes.Load())
	}
	// The other login's server is listed, and selecting it explains.
	vm.ServerSel.Set(1)
	f.settle()
	if vm.Shares.Len() != 0 || !strings.Contains(vm.Status.Get(), "another identity provider") {
		t.Fatalf("%d shares, %q", vm.Shares.Len(), vm.Status.Get())
	}
	if vm.Reload.CanExecute() || vm.Create.CanExecute() {
		t.Fatal("commands for an unreachable server")
	}
}

func TestCreateDeleteEnableDisable(t *testing.T) {
	f := newFixture(t)
	vm := f.vm
	if vm.Create.CanExecute() {
		t.Fatal("Create with an empty form")
	}
	vm.NewName.Set("music")
	vm.NewDir.Set("/srv/music")
	vm.NewSubject.Set("oidc:groups:fileshare-admins")
	vm.NewAccess.Set(1)
	if !vm.Create.CanExecute() {
		t.Fatal("Create with a full form")
	}
	vm.Create.Execute()
	f.settle()
	if got := strings.Join(f.shareNames(), ","); got != "archive,music,photos" {
		t.Fatal(got)
	}
	if s, _ := vm.share(); s.GetName() != "music" || grantLine(s.GetGrants()[0]) != "oidc:groups:fileshare-admins (write)" {
		t.Fatalf("the new share is not selected, or its grant is wrong: %v", s)
	}
	if vm.NewName.Get() != "" || vm.NewDir.Get() != "" {
		t.Fatal("the form was not cleared")
	}

	// Disable, then Enable: the commands follow the share's state.
	if !vm.Disable.CanExecute() || vm.Enable.CanExecute() {
		t.Fatal("an enabled share")
	}
	vm.Disable.Execute()
	f.settle()
	if s, _ := vm.share(); s.GetEnabled() || !vm.Enable.CanExecute() || !slices.Contains(vm.Details.Slice(), "disabled: nobody can connect") {
		t.Fatalf("after Disable: %v", s)
	}
	vm.Enable.Execute()
	f.settle()
	if s, _ := vm.share(); !s.GetEnabled() {
		t.Fatal("after Enable")
	}

	// Delete takes two presses.
	vm.Delete.Execute()
	f.settle()
	if len(f.a.Names()) != 3 || !strings.Contains(vm.Status.Get(), "Press Delete again") {
		t.Fatalf("one press deleted: %v %q", f.a.Names(), vm.Status.Get())
	}
	vm.Delete.Execute()
	f.settle()
	if got := strings.Join(f.a.Names(), ","); got != "archive,photos" {
		t.Fatal(got)
	}
	// Arming does not survive a change of selection.
	vm.Delete.Execute()
	vm.ShareSel.Set(1)
	vm.Delete.Execute()
	f.settle()
	if len(f.a.Names()) != 2 {
		t.Fatal("a press armed for one share deleted another")
	}
}

func TestGrantAndRevoke(t *testing.T) {
	f := newFixture(t)
	vm := f.vm
	vm.ShareSel.Set(1) // photos: alice, write
	vm.GrantSubject.Set("@staff")
	vm.Grant.Execute()
	f.settle()
	if vm.Grants.Len() != 2 || vm.GrantSubject.Get() != "" {
		t.Fatalf("%d grants", vm.Grants.Len())
	}
	vm.GrantSel.Set(1)
	vm.Revoke.Execute()
	f.settle()
	if vm.Grants.Len() != 1 || grantLine(vm.Grants.At(0)) != "alice (write)" {
		t.Fatalf("after revoke: %v", vm.Grants.Slice())
	}
	// The last grant: the server refuses, and the console says why.
	vm.GrantSel.Set(0)
	vm.Revoke.Execute()
	f.settle()
	if !strings.Contains(vm.Status.Get(), "the last grant") {
		t.Fatalf("status %q", vm.Status.Get())
	}
	// A subject that does not parse never reaches the server.
	vm.GrantSubject.Set("oidc:nonsense")
	calls := f.a.Calls.Load()
	vm.Grant.Execute()
	f.settle()
	if f.a.Calls.Load() != calls || !strings.HasPrefix(vm.Status.Get(), "Grant: ") {
		t.Fatalf("status %q", vm.Status.Get())
	}
	vm.NewName.Set("x")
	vm.NewDir.Set("/srv/x")
	vm.NewSubject.Set("@")
	vm.Create.Execute()
	f.settle()
	if f.a.Calls.Load() != calls || !strings.HasPrefix(vm.Status.Get(), "New share: ") {
		t.Fatalf("status %q", vm.Status.Get())
	}
}

// A server error is said; a login that ended sends the browser to log in.
func TestFailuresAndTheEndOfALogin(t *testing.T) {
	f := newFixture(t)
	vm := f.vm
	f.a.Fail = connect.NewError(connect.CodeUnavailable, "the disk is gone")
	vm.Reload.Execute()
	f.settle()
	if vm.Status.Get() != "Paris: the disk is gone" {
		t.Fatalf("status %q", vm.Status.Get())
	}
	f.a.Fail = nil
	f.fp.Logout()
	vm.Reload.Execute()
	f.settle()
	if !strings.HasSuffix(vm.NavigateTo.Get(), "/login?return=%2Fconsole%2F") {
		t.Fatalf("navigate to %q", vm.NavigateTo.Get())
	}
	// A plain error (not Connect's) is shown as it is.
	vm.fail("X", errors.New("plain"))
	if vm.Status.Get() != "X: plain" {
		t.Fatal(vm.Status.Get())
	}
}

func TestNotLoggedInAtStart(t *testing.T) {
	fp := admintest.New(false, admintest.NewServer("a", ""))
	defer fp.Close()
	q := mvvm.NewQueue(nil)
	vm := newViewModel(context.Background(), admin.NewPortal(fp.URL, nil, nil), q)
	vm.Start()
	f := &fixture{t: t, vm: vm, q: q}
	f.settle()
	if !strings.Contains(vm.NavigateTo.Get(), "/login?") {
		t.Fatalf("navigate to %q", vm.NavigateTo.Get())
	}
}

func TestNoReachableServer(t *testing.T) {
	b := admintest.NewServer("b", "")
	b.Reachable = false
	fp := admintest.New(true, b)
	defer fp.Close()
	q := mvvm.NewQueue(nil)
	vm := newViewModel(context.Background(), admin.NewPortal(fp.URL, nil, nil), q)
	vm.Start()
	(&fixture{t: t, vm: vm, q: q}).settle()
	if vm.ServerSel.Get() != -1 || !strings.Contains(vm.Status.Get(), "No server is reachable") {
		t.Fatalf("%d %q", vm.ServerSel.Get(), vm.Status.Get())
	}
}

// An answer about a server no longer selected is dropped: it would show one
// server's shares under another's name.
func TestALateAnswerForAnotherServerIsDropped(t *testing.T) {
	f := newFixture(t)
	vm := f.vm
	vm.Reload.Execute() // in flight for A
	vm.ServerSel.Set(1) // B selected before it lands
	f.settle()
	if vm.Shares.Len() != 0 {
		t.Fatalf("A's shares shown under B: %d", vm.Shares.Len())
	}
}

func TestLogout(t *testing.T) {
	f := newFixture(t)
	f.vm.Logout.Execute()
	f.settle()
	if f.vm.NavigateTo.Get() != "https://login.example.org/logout" {
		t.Fatalf("navigate to %q", f.vm.NavigateTo.Get())
	}
}

func TestSubjects(t *testing.T) {
	for in, want := range map[string]string{
		"alice": "alice", " @staff ": "@staff", "oidc:groups:hpc": "oidc:groups:hpc", "oidc:user:bob": "oidc:user:bob",
	} {
		s, err := parseSubject(in)
		if err != nil || formatSubject(s) != want {
			t.Errorf("%q: %v %q", in, err, formatSubject(s))
		}
	}
	for _, bad := range []string{"", "@", "oidc:groups:", "oidc:user:", "oidc:x"} {
		if _, err := parseSubject(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if grantLine(&v1.Grant{Subject: &v1.Subject{Kind: &v1.Subject_User{User: "a"}}, Access: v1.Access_ACCESS_WRITE}) != "a (write)" {
		t.Fatal("grantLine")
	}
}

func TestShareDescriptions(t *testing.T) {
	img := &v1.Share{Name: "disk", Source: &v1.Share_Image{Image: "/srv/disk.img"}, Origin: v1.Origin_ORIGIN_CONFIG,
		Filesystem: "fat32", EffectiveReadOnly: true, SizeBytes: 3 << 30, Protocols: []string{"smb"},
		ServedOver: []string{"smb"}, Refusals: []*v1.Refusal{{Protocol: "nfs", Reason: "no kerberos block"}},
		Grants: []*v1.Grant{{Subject: &v1.Subject{Kind: &v1.Subject_User{User: "a"}}, Access: v1.Access_ACCESS_WRITE}}}
	got := strings.Join(shareDetails(img), "|")
	for _, want := range []string{"disk image: /srv/disk.img", "configuration file", "filesystem: fat32",
		"served read-only: its source cannot take a write", "size: 3.0 GiB", "protocols: smb", "served over: smb", "not over nfs: no kerberos block", "disabled"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in %s", want, got)
		}
	}
	// Read-only because nobody may write, which is not the source's fault.
	noWrite := &v1.Share{Name: "n", Source: &v1.Share_Directory{Directory: "/d"}, EffectiveReadOnly: true, Enabled: true}
	if got := strings.Join(shareDetails(noWrite), "|"); !strings.Contains(got, "served read-only: nobody is granted write") {
		t.Error(got)
	}
	if plural(1, "share") != "1 share" || plural(2, "share") != "2 shares" {
		t.Error("plural")
	}
	vol := &v1.Share{Name: "v", Source: &v1.Share_Volume{Volume: &v1.VolumeRef{Parent: "tank", Name: "v"}}, ReadOnly: true, Enabled: true}
	if got := strings.Join(shareDetails(vol), "|"); !strings.Contains(got, "volume: volume tank/v") || !strings.Contains(got, "|read-only") {
		t.Error(got)
	}
	if shareLine(&v1.Share{Name: "x"}) != "x  ?  (disabled)" {
		t.Error(shareLine(&v1.Share{Name: "x"}))
	}
	for n, want := range map[uint64]string{512: "512 B", 1536: "1.5 KiB", 5 << 20: "5.0 MiB"} {
		if bytesString(n) != want {
			t.Errorf("%d: %s", n, bytesString(n))
		}
	}
}
