// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm

// The browser build: served by go-fileshare/portal at /console/, it reaches
// the servers through the portal, which holds the tokens.
package main

import (
	"context"
	"syscall/js"

	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/window"

	"github.com/go-fileshare/portal/client"

	"github.com/go-fileshare/console/internal/admin"
)

func main() {
	origin := js.Global().Get("location").Get("origin").String()
	// The lock every UI of the portal takes (the explorer too): they share
	// one session, so one refresh at a time across all their tabs.
	be := admin.NewPortal(origin, nil, client.WebLocks("fileshare-portal-refresh"))

	w, err := window.Open(window.Config{Title: "fileshare console", Canvas: "screen"})
	if err != nil {
		println("console:", err.Error())
		return
	}
	var wake func()
	if r, ok := w.(window.Repainter); ok {
		wake = r.Repaint
	}
	q := mvvm.NewQueue(wake)
	vm := newViewModel(context.Background(), be, q)
	v := newView(vm, q, nil)
	vm.NavigateTo.SubscribeChanged(func() {
		if to := vm.NavigateTo.Get(); to != "" {
			js.Global().Get("location").Call("assign", to)
		}
	})
	expose(vm)
	vm.Start()
	_ = w.Run(v.root)
}
