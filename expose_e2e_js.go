// SPDX-License-Identifier: BSD-3-Clause

//go:build js && wasm && e2e

package main

import (
	"encoding/json"
	"syscall/js"
)

// expose lets the browser test read the ViewModel, and only that: built with
// -tags e2e, never in the console a portal serves. It runs on the page's one
// thread, between frames, so it reads the observables where they are written.
func expose(vm *viewModel) {
	js.Global().Set("consoleState", js.FuncOf(func(js.Value, []js.Value) any {
		var shares []string
		for _, s := range vm.Shares.Slice() {
			shares = append(shares, s.GetName())
		}
		var servers []string
		for _, s := range vm.Servers.Slice() {
			servers = append(servers, s.ID)
		}
		b, _ := json.Marshal(map[string]any{"user": vm.User.Get(), "status": vm.Status.Get(),
			"servers": servers, "shares": shares, "busy": vm.Busy.Get(), "info": vm.ServerInfo.Get(),
			"volNote": vm.Vol.Note.Get(), "tab": vm.Tab.Get()})
		return string(b)
	}))
	js.Global().Set("consoleTab", js.FuncOf(func(_ js.Value, a []js.Value) any {
		vm.Tab.Set(a[0].Int())
		return nil
	}))
	// The one action the test takes through the ViewModel's own command: the
	// click path is tested natively (scene_test.go); this one tests the trip.
	js.Global().Set("consoleCreate", js.FuncOf(func(_ js.Value, a []js.Value) any {
		vm.NewName.Set(a[0].String())
		vm.NewDir.Set(a[1].String())
		vm.NewSubject.Set(a[2].String())
		vm.Create.Execute()
		return nil
	}))
}
