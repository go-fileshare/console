// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm

// The browser build: served by go-fileshare/portal at /console/, it reaches
// the servers through the portal, which holds the tokens.
package main

import (
	"context"
	"sync"
	"syscall/js"

	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/window"

	"github.com/go-fileshare/console/internal/admin"
)

func main() {
	origin := js.Global().Get("location").Get("origin").String()
	be := admin.NewPortal(origin, nil, webLocks)

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

var tabMu sync.Mutex

// webLocks runs fn while holding a lock every tab of this origin shares (the
// Web Locks API), so that two tabs never refresh at once: a provider that
// rotates refresh tokens ends the login when one is spent twice. A browser
// without the API gets this tab's own lock.
func webLocks(fn func() error) error {
	locks := js.Global().Get("navigator").Get("locks")
	if !locks.Truthy() {
		tabMu.Lock()
		defer tabMu.Unlock()
		return fn()
	}
	done := make(chan error, 1)
	var inner js.Func
	outer := js.FuncOf(func(js.Value, []js.Value) any {
		// The lock is held until the promise returned here settles.
		inner = js.FuncOf(func(_ js.Value, a []js.Value) any {
			resolve := a[0]
			go func() {
				err := fn()
				resolve.Invoke()
				done <- err
			}()
			return nil
		})
		return js.Global().Get("Promise").New(inner)
	})
	defer outer.Release()
	locks.Call("request", "fileshare-portal-refresh", outer)
	err := <-done
	inner.Release()
	return err
}
