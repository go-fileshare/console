// SPDX-License-Identifier: BSD-3-Clause
//
// The ViewModel: ALL of the console's state, as go-widgets/mvvm primitives,
// and never a widget. The View (scene.go) binds widgets to it through
// go-widgets/mvvmtk. It is tested natively (viewmodel_test.go) against the
// real portal client and a fake portal, with no canvas.
//
// Every call to a server runs on its own goroutine and comes back through
// an mvvm.Queue: observables are not safe for concurrent use, so the result
// is applied on the UI goroutine, at the start of the next frame.

package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"connectrpc.com/connect/v2"
	"github.com/go-widgets/mvvm"

	v1 "github.com/go-fileshare/console/internal/adminpb"

	"github.com/go-fileshare/console/internal/admin"
)

// The pages.
const (
	tabShares = iota
	tabVolumes
)

// accessNames are the access levels a grant can give, as the DropDowns show
// them; the index is the choice.
var accessNames = []string{"read", "write"}

type viewModel struct {
	be  admin.Backend
	q   *mvvm.Queue
	ctx context.Context

	// Who is logged in, and what the console is doing or has to say.
	User   *mvvm.Observable[string]
	Status *mvvm.Observable[string]
	// NavigateTo is set when the browser must leave the console: to log in,
	// or to end the session at the identity provider.
	NavigateTo *mvvm.Observable[string]

	Servers    *mvvm.ObservableList[admin.Server]
	ServerSel  *mvvm.Observable[int]
	ServerInfo *mvvm.Observable[string]

	Shares   *mvvm.ObservableList[*v1.Share]
	ShareSel *mvvm.Observable[int]
	Details  *mvvm.ObservableList[string]
	Grants   *mvvm.ObservableList[*v1.Grant]
	GrantSel *mvvm.Observable[int]

	// The new-share form, and the grant form.
	NewName, NewDir, NewSubject *mvvm.Observable[string]
	NewAccess                   *mvvm.Observable[int]
	GrantSubject                *mvvm.Observable[string]
	GrantAccess                 *mvvm.Observable[int]

	Reload, Create, Delete, Enable, Disable, Grant, Revoke, Logout *mvvm.Command

	// Tab is the page shown: shares or volumes.
	Tab *mvvm.Observable[int]
	// Vol is the Volumes page (volumes.go).
	Vol *volumeVM

	// Pending counts calls in flight; Busy says so to the View.
	pending int
	Busy    *mvvm.Observable[bool]
	// armed is the share a first Delete armed: the second deletes it.
	armed string
	// generation discards answers about a server no longer selected.
	generation int
}

func newViewModel(ctx context.Context, be admin.Backend, q *mvvm.Queue) *viewModel {
	vm := &viewModel{be: be, q: q, ctx: ctx,
		User: mvvm.NewObservable(""), Status: mvvm.NewObservable("Loading..."), NavigateTo: mvvm.NewObservable(""),
		Servers: mvvm.NewObservableList[admin.Server](), ServerSel: mvvm.NewObservable(-1), ServerInfo: mvvm.NewObservable(""),
		Shares: mvvm.NewObservableList[*v1.Share](), ShareSel: mvvm.NewObservable(-1),
		Details: mvvm.NewObservableList[string](), Grants: mvvm.NewObservableList[*v1.Grant](), GrantSel: mvvm.NewObservable(-1),
		NewName: mvvm.NewObservable(""), NewDir: mvvm.NewObservable(""), NewSubject: mvvm.NewObservable(""),
		NewAccess: mvvm.NewObservable(0), GrantSubject: mvvm.NewObservable(""), GrantAccess: mvvm.NewObservable(0),
		Busy: mvvm.NewObservable(false), Tab: mvvm.NewObservable(tabShares), Vol: newVolumeVM(),
	}
	server := func() bool { _, ok := vm.server(); return ok }
	share := func() bool { _, ok := vm.share(); return ok }
	vm.Reload = mvvm.NewCommand(vm.loadServer, server)
	vm.Create = mvvm.NewCommand(vm.create, func() bool {
		return server() && strings.TrimSpace(vm.NewName.Get()) != "" && strings.TrimSpace(vm.NewDir.Get()) != "" &&
			strings.TrimSpace(vm.NewSubject.Get()) != ""
	})
	vm.Delete = mvvm.NewCommand(vm.delete, share)
	vm.Enable = mvvm.NewCommand(func() { vm.setEnabled(true) }, func() bool { s, ok := vm.share(); return ok && !s.GetEnabled() })
	vm.Disable = mvvm.NewCommand(func() { vm.setEnabled(false) }, func() bool { s, ok := vm.share(); return ok && s.GetEnabled() })
	vm.Grant = mvvm.NewCommand(vm.grant, func() bool { return share() && strings.TrimSpace(vm.GrantSubject.Get()) != "" })
	vm.Revoke = mvvm.NewCommand(vm.revoke, func() bool { _, ok := vm.grantSelected(); return ok })
	vm.Logout = mvvm.NewCommand(vm.logout, nil)

	mvvm.BindCanExecute(vm.Reload, vm.ServerSel, vm.Servers)
	mvvm.BindCanExecute(vm.Create, vm.ServerSel, vm.Servers, vm.NewName, vm.NewDir, vm.NewSubject)
	mvvm.BindCanExecute(vm.Delete, vm.ShareSel, vm.Shares)
	mvvm.BindCanExecute(vm.Enable, vm.ShareSel, vm.Shares)
	mvvm.BindCanExecute(vm.Disable, vm.ShareSel, vm.Shares)
	mvvm.BindCanExecute(vm.Grant, vm.ShareSel, vm.Shares, vm.GrantSubject)
	mvvm.BindCanExecute(vm.Revoke, vm.GrantSel, vm.Grants)

	vm.wireVolumes()

	vm.ServerSel.SubscribeChanged(vm.loadServer)
	vm.ShareSel.SubscribeChanged(vm.showShare)
	return vm
}

// --- asynchronous calls ---------------------------------------------------

// async runs fn on its own goroutine and applies its result on the UI
// goroutine. A result about a server no longer selected is dropped.
func async[T any](vm *viewModel, what string, fn func(ctx context.Context) (T, error), apply func(T)) {
	vm.pending++
	vm.Busy.Set(true)
	gen := vm.generation
	go func() {
		v, err := fn(vm.ctx)
		vm.q.Post(func() {
			vm.pending--
			vm.Busy.Set(vm.pending > 0)
			if gen != vm.generation {
				return
			}
			if err != nil {
				vm.fail(what, err)
				return
			}
			apply(v)
		})
	}()
}

// fail says what went wrong; an ended login sends the browser to log in.
func (vm *viewModel) fail(what string, err error) {
	if errors.Is(err, admin.ErrNotLoggedIn) || connect.CodeOf(err) == connect.CodeUnauthenticated {
		vm.Status.Set("You are not logged in, or your session has ended.")
		vm.NavigateTo.Set(vm.be.LoginURL("/console/"))
		return
	}
	msg := err.Error()
	var ce *connect.Error
	if errors.As(err, &ce) {
		msg = ce.Message()
	}
	vm.Status.Set(what + ": " + msg)
}

// Start asks who is logged in and lists the servers.
func (vm *viewModel) Start() {
	async(vm, "Session", vm.be.Session, func(s *admin.Session) {
		vm.User.Set(s.Who())
		vm.Servers.Clear()
		vm.Servers.Append(s.Servers...)
		vm.Status.Set("")
		first := slices.IndexFunc(s.Servers, func(sv admin.Server) bool { return sv.Reachable })
		if first < 0 {
			vm.Status.Set("No server is reachable from this login.")
			return
		}
		vm.ServerSel.Set(first)
	})
}

// --- selection ----------------------------------------------------------------

func (vm *viewModel) server() (admin.Server, bool) {
	i := vm.ServerSel.Get()
	if i < 0 || i >= vm.Servers.Len() {
		return admin.Server{}, false
	}
	s := vm.Servers.At(i)
	return s, s.Reachable
}

func (vm *viewModel) share() (*v1.Share, bool) {
	i := vm.ShareSel.Get()
	if i < 0 || i >= vm.Shares.Len() {
		return nil, false
	}
	return vm.Shares.At(i), true
}

func (vm *viewModel) grantSelected() (*v1.Grant, bool) {
	i := vm.GrantSel.Get()
	if i < 0 || i >= vm.Grants.Len() {
		return nil, false
	}
	return vm.Grants.At(i), true
}

// loadServer reads the selected server's state and shares.
func (vm *viewModel) loadServer() {
	vm.generation++
	vm.armed = ""
	s, ok := vm.server()
	if !ok {
		vm.ServerInfo.Set("")
		vm.setShares(nil, "")
		vm.loadVolumes("")
		if i := vm.ServerSel.Get(); i >= 0 && i < vm.Servers.Len() {
			vm.Status.Set(vm.Servers.At(i).Name() + " is behind another identity provider: log in there to manage it.")
		}
		return
	}
	keep := ""
	if sh, ok := vm.share(); ok {
		keep = sh.GetName()
	}
	vm.loadShares(s, keep)
	vm.reloadVolumes()
}

type serverState struct {
	info   *v1.ServerInfo
	shares []*v1.Share
}

func (vm *viewModel) loadShares(s admin.Server, keep string) {
	c := vm.be.Client(s.ID)
	async(vm, s.Name(), func(ctx context.Context) (serverState, error) {
		info, err := c.GetServerInfo(ctx, &v1.GetServerInfoRequest{})
		if err != nil {
			return serverState{}, err
		}
		list, err := c.ListShares(ctx, &v1.ListSharesRequest{})
		if err != nil {
			return serverState{}, err
		}
		return serverState{info.GetInfo(), list.GetShares()}, nil
	}, func(st serverState) {
		vm.ServerInfo.Set(fmt.Sprintf("%s - fileshare %s, %s", st.info.GetName(), st.info.GetVersion(), plural(len(st.shares), "share")))
		vm.setShares(st.shares, keep)
	})
}

// setShares replaces the list, keeping the share named keep selected.
func (vm *viewModel) setShares(shares []*v1.Share, keep string) {
	vm.Shares.Clear()
	vm.Shares.Append(shares...)
	sel := slices.IndexFunc(shares, func(s *v1.Share) bool { return s.GetName() == keep })
	if sel < 0 && len(shares) > 0 {
		sel = 0
	}
	if vm.ShareSel.Get() == sel {
		vm.showShare() // the same index, another share: show it
	}
	vm.ShareSel.Set(sel)
}

// showShare derives the detail lines and the grants of the selected share.
func (vm *viewModel) showShare() {
	vm.armed = ""
	vm.Details.Clear()
	vm.Grants.Clear()
	vm.GrantSel.Set(-1)
	s, ok := vm.share()
	if !ok {
		return
	}
	vm.Details.Append(shareDetails(s)...)
	vm.Grants.Append(s.GetGrants()...)
	if len(s.GetGrants()) > 0 {
		vm.GrantSel.Set(0)
	}
}

// replaceShare puts a share a call returned in place of its old copy.
func (vm *viewModel) replaceShare(s *v1.Share) {
	list := slices.Clone(vm.Shares.Slice())
	for i, old := range list {
		if old.GetName() == s.GetName() {
			list[i] = s
		}
	}
	vm.setShares(list, s.GetName())
}

// --- commands -----------------------------------------------------------------

func (vm *viewModel) create() {
	sv, _ := vm.server()
	name := strings.TrimSpace(vm.NewName.Get())
	subj, err := parseSubject(vm.NewSubject.Get())
	if err != nil {
		vm.Status.Set("New share: " + err.Error())
		return
	}
	req, err := shareSource(vm.NewDir.Get())
	if err != nil {
		vm.Status.Set("New share: " + err.Error())
		return
	}
	req.Name = name
	req.Grants = []*v1.Grant{{Subject: subj, Access: access(vm.NewAccess.Get())}}
	c := vm.be.Client(sv.ID)
	async(vm, "Create "+name, func(ctx context.Context) (*v1.CreateShareResponse, error) {
		return c.CreateShare(ctx, req)
	}, func(*v1.CreateShareResponse) {
		vm.NewName.Set("")
		vm.NewDir.Set("")
		vm.Status.Set("Share " + name + " created.")
		vm.loadShares(sv, name)
		vm.reloadVolumes() // a volume's shares changed
	})
}

// delete is two presses: the first arms it and says what will go.
func (vm *viewModel) delete() {
	sv, _ := vm.server()
	s, _ := vm.share()
	name := s.GetName()
	if vm.armed != name {
		vm.armed = name
		vm.Status.Set("Press Delete again to delete share " + name + ". Its files stay where they are.")
		return
	}
	vm.armed = ""
	c := vm.be.Client(sv.ID)
	async(vm, "Delete "+name, func(ctx context.Context) (*v1.DeleteShareResponse, error) {
		return c.DeleteShare(ctx, &v1.DeleteShareRequest{Name: name})
	}, func(*v1.DeleteShareResponse) {
		vm.Status.Set("Share " + name + " deleted.")
		vm.loadShares(sv, "")
		vm.reloadVolumes() // a volume may be free now
	})
}

func (vm *viewModel) setEnabled(on bool) {
	sv, _ := vm.server()
	s, _ := vm.share()
	name := s.GetName()
	c := vm.be.Client(sv.ID)
	if on {
		async(vm, "Enable "+name, func(ctx context.Context) (*v1.EnableShareResponse, error) {
			return c.EnableShare(ctx, &v1.EnableShareRequest{Name: name})
		}, func(r *v1.EnableShareResponse) {
			vm.Status.Set("Share " + name + " enabled.")
			vm.replaceShare(r.GetShare())
		})
		return
	}
	async(vm, "Disable "+name, func(ctx context.Context) (*v1.DisableShareResponse, error) {
		return c.DisableShare(ctx, &v1.DisableShareRequest{Name: name})
	}, func(r *v1.DisableShareResponse) {
		vm.Status.Set("Share " + name + " disabled: its connections were closed.")
		vm.replaceShare(r.GetShare())
	})
}

func (vm *viewModel) grant() {
	sv, _ := vm.server()
	s, _ := vm.share()
	subj, err := parseSubject(vm.GrantSubject.Get())
	if err != nil {
		vm.Status.Set("Grant: " + err.Error())
		return
	}
	name := s.GetName()
	req := &v1.GrantRequest{Share: name, Grant: &v1.Grant{Subject: subj, Access: access(vm.GrantAccess.Get())}}
	c := vm.be.Client(sv.ID)
	async(vm, "Grant on "+name, func(ctx context.Context) (*v1.GrantResponse, error) {
		return c.Grant(ctx, req)
	}, func(r *v1.GrantResponse) {
		vm.GrantSubject.Set("")
		vm.Status.Set(formatSubject(subj) + " may now use " + name + ".")
		vm.replaceShare(r.GetShare())
	})
}

func (vm *viewModel) revoke() {
	sv, _ := vm.server()
	s, _ := vm.share()
	g, _ := vm.grantSelected()
	name := s.GetName()
	c := vm.be.Client(sv.ID)
	async(vm, "Revoke on "+name, func(ctx context.Context) (*v1.RevokeResponse, error) {
		return c.Revoke(ctx, &v1.RevokeRequest{Share: name, Subject: g.GetSubject()})
	}, func(r *v1.RevokeResponse) {
		vm.Status.Set(formatSubject(g.GetSubject()) + " may no longer use " + name + ".")
		vm.replaceShare(r.GetShare())
	})
}

func (vm *viewModel) logout() {
	async(vm, "Log out", vm.be.Logout, func(end string) {
		vm.Status.Set("Logged out.")
		if end == "" {
			end = vm.be.LoginURL("/console/")
		}
		vm.NavigateTo.Set(end)
	})
}

func access(i int) v1.Access {
	if i == 1 {
		return v1.Access_ACCESS_WRITE
	}
	return v1.Access_ACCESS_READ
}

// plural is "1 share", "2 shares".
func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
