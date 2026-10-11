// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"context"
	"github.com/go-fileshare/console/internal/admin"
	"github.com/go-fileshare/console/internal/admin/admintest"
	"github.com/go-widgets/mvvm"
)

const nativeBase = `
issuer "https://login.example.org" {
  client_id = "fileshare-console"
}
server "paris" {
  label    = "Paris"
  url      = "https://fs-paris.example.org:8443"
  resource = "https://fs-paris.example.org/"
  ca_file  = "ca.pem"
}
`

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "console.hcl")
	if err := os.WriteFile(p, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNativeConfig(t *testing.T) {
	p := writeConfig(t, nativeBase)
	c, err := loadNative(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Issuer != "https://login.example.org" || c.ClientID != "fileshare-console" || len(c.Servers) != 1 {
		t.Fatalf("%+v", c)
	}
	s := c.Servers[0]
	if s.Issuer != c.Issuer || s.Resource != "https://fs-paris.example.org/" || s.CAFile != filepath.Join(filepath.Dir(p), "ca.pem") {
		t.Fatalf("%+v", s)
	}
	for name, change := range map[string][2]string{
		"not HCL":         {nativeBase, nativeBase + "}"},
		"unknown field":   {nativeBase, nativeBase + "x = 1\n"},
		"issuer in clear": {`"https://login.example.org"`, `"http://login.example.org"`},
		"server in clear": {`"https://fs-paris.example.org:8443"`, `"http://fs-paris.example.org:8443"`},
		"no client_id":    {`client_id = "fileshare-console"`, ``},
	} {
		text := strings.Replace(nativeBase, change[0], change[1], 1)
		if text == nativeBase && change[0] != nativeBase {
			t.Fatalf("%s: the case changes nothing", name)
		}
		if _, err := loadNative(writeConfig(t, text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := loadNative(filepath.Join(t.TempDir(), "none.hcl")); err == nil {
		t.Error("a missing file loaded")
	}
	if !strings.HasSuffix(defaultConfigPath(), filepath.Join("fileshare-console", "console.hcl")) {
		t.Error(defaultConfigPath())
	}
}

// Natively, "log in again" starts again -- the backend opens the browser --
// and anything else is left alone.
func TestNavigate(t *testing.T) {
	fp := admintest.New(true, admintest.NewServer("a", ""))
	defer fp.Close()
	q := mvvm.NewQueue(nil)
	vm := newViewModel(context.Background(), admin.NewPortal(fp.URL, nil, nil), q)
	vm.NavigateTo.Set(admin.LoginAgain)
	navigate(vm, admin.LoginAgain)
	if vm.NavigateTo.Get() != "" || vm.pending != 1 {
		t.Fatalf("navigate %q, %d calls in flight", vm.NavigateTo.Get(), vm.pending)
	}
	(&fixture{t: t, vm: vm, q: q}).settle()
	navigate(vm, "native:other")
	if vm.pending != 0 {
		t.Fatal("an unknown destination started something")
	}
}
