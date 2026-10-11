// SPDX-License-Identifier: BSD-3-Clause
//
// The View: go-widgets/toolkit widgets in box layouts, each bound to the
// ViewModel through go-widgets/mvvmtk. It never writes a widget's state field
// -- the binders own those -- so it passes go-widgets/mvvmlint. It carries no
// build tag: scene_test.go builds, binds, lays out and draws it natively.
//
// The same tree runs in a browser tab and as a native window: window.Open
// picks the back-end, and Run drives this tree on either.

package main

import (
	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/mvvmtk"
	"github.com/go-widgets/toolkit"

	"github.com/go-fileshare/console/internal/admin"
)

// root is the top of the tree: a column whose SetBounds -- which every
// window back-end calls before it draws -- first applies the results that
// arrived on other goroutines. That is what puts them on the screen, and on
// the UI goroutine.
type root struct {
	*toolkit.VBox
	q *mvvm.Queue
}

func (r *root) SetBounds(b toolkit.Rect) {
	r.q.Drain()
	r.VBox.SetBounds(b)
}

const (
	rowH    = 30
	titleH  = 22
	statusH = 24
	sideW   = 220
	buttonW = 96
	accessW = 90
)

// view is the widget tree and what it is bound to.
type view struct {
	root *root

	servers, shares, details, grants *toolkit.ListBox
	user, info, status               *toolkit.Label
	newName, newDir, newSubject      *toolkit.Entry
	grantSubject                     *toolkit.Entry
	newAccess, grantAccess           *toolkit.DropDown
	reload, enable, disable, del     *toolkit.Button
	create, grant, revoke, logout    *toolkit.Button
	tabs                             *toolkit.Notebook

	// The Volumes page.
	volumes, volDetails                  *toolkit.ListBox
	volNote                              *toolkit.Label
	volName, volSize, volResize, volSnap *toolkit.Entry
	volParent                            *toolkit.DropDown
	volCreate, volResizeB, volSnapB      *toolkit.Button
	volDelete, volShare                  *toolkit.Button
}

// newView builds the tree for vm. invalidate (may be nil) asks for a frame
// when a binding changes a widget.
func newView(vm *viewModel, q *mvvm.Queue, invalidate func()) *view {
	v := &view{
		servers: toolkit.NewListBox(nil), shares: toolkit.NewListBox(nil),
		details: toolkit.NewListBox(nil), grants: toolkit.NewListBox(nil),
		user: toolkit.NewLabel(""), info: toolkit.NewLabel(""), status: toolkit.NewLabel(""),
		newName: toolkit.NewEntry(""), newDir: toolkit.NewEntry(""), newSubject: toolkit.NewEntry(""),
		grantSubject: toolkit.NewEntry(""),
		newAccess:    toolkit.NewDropDown(accessNames, 0), grantAccess: toolkit.NewDropDown(accessNames, 0),
		reload: toolkit.NewButton("Reload", nil), enable: toolkit.NewButton("Enable", nil),
		disable: toolkit.NewButton("Disable", nil), del: toolkit.NewButton("Delete", nil),
		create: toolkit.NewButton("Create", nil), grant: toolkit.NewButton("Grant", nil),
		revoke: toolkit.NewButton("Revoke", nil), logout: toolkit.NewButton("Log out", nil),
		tabs:    toolkit.NewNotebook(),
		volumes: toolkit.NewListBox(nil), volDetails: toolkit.NewListBox(nil), volNote: toolkit.NewLabel(""),
		volName: toolkit.NewEntry(""), volSize: toolkit.NewEntry(""), volResize: toolkit.NewEntry(""), volSnap: toolkit.NewEntry(""),
		volParent: toolkit.NewDropDown(nil, 0),
		volCreate: toolkit.NewButton("Create", nil), volResizeB: toolkit.NewButton("Resize", nil),
		volSnapB: toolkit.NewButton("Snapshot", nil), volDelete: toolkit.NewButton("Delete", nil),
		volShare: toolkit.NewButton("Share it", nil),
	}
	v.volName.Placeholder = "name"
	v.volSize.Placeholder = "size: 500G, 2T"
	v.volResize.Placeholder = "new size"
	v.volSnap.Placeholder = "snapshot name"
	// Lines that can be longer than their column end in an ellipsis rather
	// than run over the next one.
	for _, l := range []*toolkit.Label{v.info, v.volNote, v.status, v.user} {
		l.Ellipsis = true
	}
	v.newName.Placeholder = "name"
	v.newDir.Placeholder = "directory, under the server source_roots"
	v.newSubject.Placeholder = "who: alice, @staff, oidc:groups:..."
	v.grantSubject.Placeholder = "who: alice, @staff, oidc:groups:..."

	// Bindings: every piece of widget state comes from the ViewModel.
	mvvmtk.BindListItems(v.servers, vm.Servers, func(s admin.Server) string {
		if !s.Reachable {
			return s.Name() + "  (other login)"
		}
		return s.Name()
	}, invalidate)
	mvvmtk.BindListSelection(v.servers, vm.ServerSel, invalidate)
	mvvmtk.BindListItems(v.shares, vm.Shares, shareLine, invalidate)
	mvvmtk.BindListSelection(v.shares, vm.ShareSel, invalidate)
	mvvmtk.BindListItems(v.details, vm.Details, func(s string) string { return s }, invalidate)
	mvvmtk.BindListItems(v.grants, vm.Grants, grantLine, invalidate)
	mvvmtk.BindListSelection(v.grants, vm.GrantSel, invalidate)
	mvvmtk.BindLabel(v.user, vm.User, invalidate)
	mvvmtk.BindLabel(v.info, vm.ServerInfo, invalidate)
	mvvmtk.BindLabel(v.status, vm.Status, invalidate)
	mvvmtk.BindEntryText(v.newName, vm.NewName, invalidate)
	mvvmtk.BindEntryText(v.newDir, vm.NewDir, invalidate)
	mvvmtk.BindEntryText(v.newSubject, vm.NewSubject, invalidate)
	mvvmtk.BindEntryText(v.grantSubject, vm.GrantSubject, invalidate)
	mvvmtk.BindSelectedIndex(v.newAccess, vm.NewAccess, invalidate)
	mvvmtk.BindSelectedIndex(v.grantAccess, vm.GrantAccess, invalidate)
	vol := vm.Vol
	mvvmtk.BindNotebook(v.tabs, vm.Tab, invalidate)
	mvvmtk.BindLabel(v.volNote, vol.Note, invalidate)
	mvvmtk.BindListItems(v.volumes, vol.Volumes, volumeLine, invalidate)
	mvvmtk.BindListSelection(v.volumes, vol.VolSel, invalidate)
	mvvmtk.BindListItems(v.volDetails, vol.Details, func(s string) string { return s }, invalidate)
	mvvmtk.BindDropDownOptions(v.volParent, vol.Parents, parentLine, invalidate)
	mvvmtk.BindSelectedIndex(v.volParent, vol.NewParent, invalidate)
	mvvmtk.BindEntryText(v.volName, vol.NewName, invalidate)
	mvvmtk.BindEntryText(v.volSize, vol.NewSize, invalidate)
	mvvmtk.BindEntryText(v.volResize, vol.Resize, invalidate)
	mvvmtk.BindEntryText(v.volSnap, vol.SnapName, invalidate)
	for b, c := range map[*toolkit.Button]*mvvm.Command{
		v.volCreate: vol.Create, v.volResizeB: vol.DoResize, v.volSnapB: vol.Snapshot,
		v.volDelete: vol.Delete, v.volShare: vol.Share,
	} {
		mvvmtk.BindCommand(b, c, invalidate)
	}
	for b, c := range map[*toolkit.Button]*mvvm.Command{
		v.reload: vm.Reload, v.enable: vm.Enable, v.disable: vm.Disable, v.del: vm.Delete,
		v.create: vm.Create, v.grant: vm.Grant, v.revoke: vm.Revoke, v.logout: vm.Logout,
	} {
		mvvmtk.BindCommand(b, c, invalidate)
	}

	// Layout.
	header := toolkit.NewHBox()
	header.Append(toolkit.NewLabel("fileshare console"))
	header.AddFixed(v.user, 240)
	header.AddFixed(v.logout, buttonW)

	left := toolkit.NewVBox()
	left.AddFixed(toolkit.NewLabel("Servers"), titleH)
	left.Append(v.servers)

	actions := toolkit.NewHBox()
	for _, b := range []*toolkit.Button{v.reload, v.enable, v.disable, v.del} {
		actions.AddFixed(b, buttonW)
	}
	newShare := toolkit.NewHBox()
	newShare.Append(v.newName)
	newShare.AddFlex(v.newDir, 2)
	newShare.Append(v.newSubject)
	newShare.AddFixed(v.newAccess, accessW)
	newShare.AddFixed(v.create, buttonW)
	middle := toolkit.NewVBox()
	middle.AddFixed(v.info, titleH)
	middle.Append(v.shares)
	middle.AddFixed(actions, rowH)
	middle.AddFixed(toolkit.NewLabel("New share"), titleH)
	middle.AddFixed(newShare, rowH)

	grantRow := toolkit.NewHBox()
	grantRow.Append(v.grantSubject)
	grantRow.AddFixed(v.grantAccess, accessW)
	grantRow.AddFixed(v.grant, buttonW)
	grantRow.AddFixed(v.revoke, buttonW)
	right := toolkit.NewVBox()
	right.AddFixed(toolkit.NewLabel("Details"), titleH)
	right.Append(v.details)
	right.AddFixed(toolkit.NewLabel("Who may use it"), titleH)
	right.Append(v.grants)
	right.AddFixed(grantRow, rowH)

	sharesPage := toolkit.NewHBox()
	sharesPage.AddFlex(middle, 3)
	sharesPage.AddFlex(right, 2)

	volActions := toolkit.NewHBox()
	volActions.Append(v.volResize)
	volActions.AddFixed(v.volResizeB, buttonW)
	volActions.Append(v.volSnap)
	volActions.AddFixed(v.volSnapB, buttonW)
	volActions.AddFixed(v.volShare, buttonW)
	volActions.AddFixed(v.volDelete, buttonW)
	newVol := toolkit.NewHBox()
	newVol.Append(v.volName)
	newVol.AddFlex(v.volParent, 2)
	newVol.Append(v.volSize)
	newVol.AddFixed(v.volCreate, buttonW)
	volList := toolkit.NewVBox()
	volList.AddFixed(v.volNote, titleH)
	volList.Append(v.volumes)
	volList.AddFixed(volActions, rowH)
	volList.AddFixed(toolkit.NewLabel("New volume"), titleH)
	volList.AddFixed(newVol, rowH)
	volRight := toolkit.NewVBox()
	volRight.AddFixed(toolkit.NewLabel("Details"), titleH)
	volRight.Append(v.volDetails)
	volumesPage := toolkit.NewHBox()
	volumesPage.AddFlex(volList, 3)
	volumesPage.AddFlex(volRight, 2)

	v.tabs.AddTab("Shares", sharesPage)
	v.tabs.AddTab("Volumes", volumesPage)

	body := toolkit.NewHBox()
	body.AddFixed(left, sideW)
	body.Append(v.tabs)

	col := toolkit.NewVBox()
	col.AddFixed(header, rowH)
	col.Append(body)
	col.AddFixed(v.status, statusH)
	v.root = &root{VBox: col, q: q}
	return v
}
