// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

// The native build: it logs in through the system browser (go-authn/rp,
// RFC 8252) and calls each server with a token of its own, with no portal in
// between. -demo opens it over a built-in stand-in instead, to see it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/go-authn/rp"
	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/window"

	"github.com/go-fileshare/console/internal/admin"
	"github.com/go-fileshare/console/internal/admin/admintest"
)

func main() {
	demo := flag.Bool("demo", false, "run over a built-in fake portal and two fake servers")
	config := flag.String("config", defaultConfigPath(), "the servers, and where to log in")
	flag.Parse()
	var be admin.Backend
	if *demo {
		fp := admintest.New(true,
			admintest.NewServer("paris", "Paris", admintest.Share("photos", "/srv/photos", "alice", true),
				admintest.Share("archive", "/srv/archive", "@staff", false)),
			admintest.NewServer("lyon", "Lyon", admintest.Share("scratch", "/srv/scratch", "oidc:groups:hpc", true)))
		defer fp.Close()
		be = admin.NewPortal(fp.URL, nil, nil)
	} else {
		cfg, err := loadNative(*config)
		if err != nil {
			fmt.Fprintf(os.Stderr, "console: %v\n(the servers and the identity provider are read from %s; -demo runs without them)\n", err, *config)
			os.Exit(2)
		}
		d, err := admin.NewDirect(cfg, rp.OpenBrowser, nil)
		if err != nil {
			fmt.Fprintln(os.Stderr, "console:", err)
			os.Exit(2)
		}
		be = d
	}

	w, err := window.Open(window.Config{Title: "fileshare console", Width: 1100, Height: 640})
	if err != nil {
		fmt.Fprintln(os.Stderr, "console:", err)
		os.Exit(1)
	}
	var wake func()
	if r, ok := w.(window.Repainter); ok {
		wake = r.Repaint
	}
	q := mvvm.NewQueue(wake)
	vm := newViewModel(context.Background(), be, q)
	v := newView(vm, q, nil)
	vm.NavigateTo.SubscribeChanged(func() { navigate(vm, vm.NavigateTo.Get()) })
	if !*demo {
		vm.Status.Set("Log in in your browser...")
	}
	vm.Start()
	if err := w.Run(v.root); err != nil {
		fmt.Fprintln(os.Stderr, "console:", err)
		os.Exit(1)
	}
}

// navigate is where a browser page would go: natively, logging in again is
// starting again -- the backend opens the browser -- and a provider's
// end-of-session page opens in the browser.
func navigate(vm *viewModel, to string) {
	switch {
	case to == admin.LoginAgain:
		vm.NavigateTo.Set("")
		vm.Status.Set("Log in in your browser...")
		vm.Start()
	case strings.HasPrefix(to, "https://"):
		vm.NavigateTo.Set("")
		_ = rp.OpenBrowser(to)
	}
}
