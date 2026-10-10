// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

// The native build. It logs in and reaches the servers on its own, without a
// portal -- which is the next step; until then, -demo opens the console over
// a built-in fake portal and two fake servers, to see and try it.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/go-widgets/mvvm"
	"github.com/go-widgets/window"

	"github.com/go-fileshare/console/internal/admin"
	"github.com/go-fileshare/console/internal/admin/admintest"
)

func main() {
	demo := flag.Bool("demo", false, "run over a built-in fake portal and two fake servers")
	flag.Parse()
	if !*demo {
		fmt.Fprintln(os.Stderr, "console: the native console cannot log in on its own yet; run it in a browser, served by go-fileshare/portal, or try it with -demo")
		os.Exit(2)
	}
	fp := admintest.New(true,
		admintest.NewServer("paris", "Paris", admintest.Share("photos", "/srv/photos", "alice", true),
			admintest.Share("archive", "/srv/archive", "@staff", false)),
		admintest.NewServer("lyon", "Lyon", admintest.Share("scratch", "/srv/scratch", "oidc:groups:hpc", true)))
	defer fp.Close()
	be := admin.NewPortal(fp.URL, nil, nil)

	w, err := window.Open(window.Config{Title: "fileshare console (demo)", Width: 1100, Height: 640})
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
	vm.Start()
	if err := w.Run(v.root); err != nil {
		fmt.Fprintln(os.Stderr, "console:", err)
		os.Exit(1)
	}
}
