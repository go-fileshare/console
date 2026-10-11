// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/gohcl"
	"github.com/hashicorp/hcl/v2/hclparse"

	"github.com/go-fileshare/console/internal/admin"
)

// The native console's configuration: where to log in, and the servers --
// what a portal would know.
//
//	issuer "https://login.example.org" {
//	  client_id = "fileshare-console"     # a PUBLIC client: no secret
//	}
//
//	server "paris" {
//	  label    = "Paris"
//	  url      = "https://fs-paris.example.org:8443"   # fileshare's admin web listener
//	  resource = "https://fs-paris.example.org/"       # that server's audience
//	}
type nativeConfig struct {
	Issuer  issuerConf   `hcl:"issuer,block"`
	Servers []serverConf `hcl:"server,block"`
}

type issuerConf struct {
	URL      string   `hcl:"url,label"`
	ClientID string   `hcl:"client_id"`
	Scopes   []string `hcl:"scopes,optional"`
}

type serverConf struct {
	ID       string `hcl:"id,label"`
	Label    string `hcl:"label,optional"`
	URL      string `hcl:"url"`
	Resource string `hcl:"resource,optional"`
	Scope    string `hcl:"scope,optional"`
	CAFile   string `hcl:"ca_file,optional"`
}

// defaultConfigPath is console.hcl in the user's configuration directory:
// ~/Library/Application Support/fileshare-console on macOS,
// $XDG_CONFIG_HOME/fileshare-console elsewhere.
func defaultConfigPath() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return "console.hcl"
	}
	return filepath.Join(d, "fileshare-console", "console.hcl")
}

// loadNative reads path into the Direct backend's configuration.
func loadNative(path string) (admin.DirectConfig, error) {
	parser := hclparse.NewParser()
	f, diags := parser.ParseHCLFile(path)
	if diags.HasErrors() {
		return admin.DirectConfig{}, diagError(parser, diags)
	}
	var c nativeConfig
	if diags := gohcl.DecodeBody(f.Body, nil, &c); diags.HasErrors() {
		return admin.DirectConfig{}, diagError(parser, diags)
	}
	if !strings.HasPrefix(c.Issuer.URL, "https://") && !strings.HasPrefix(c.Issuer.URL, "http://127.0.0.1") {
		return admin.DirectConfig{}, fmt.Errorf("%s: issuer %q is not https", path, c.Issuer.URL)
	}
	out := admin.DirectConfig{Issuer: c.Issuer.URL, ClientID: c.Issuer.ClientID, Scopes: c.Issuer.Scopes}
	for _, s := range c.Servers {
		if !strings.HasPrefix(s.URL, "https://") {
			// ⛔ A bearer token over cleartext is anybody's.
			return admin.DirectConfig{}, fmt.Errorf("%s: server %q: url %q is not https", path, s.ID, s.URL)
		}
		ca := s.CAFile
		if ca != "" && !filepath.IsAbs(ca) {
			ca = filepath.Join(filepath.Dir(path), ca)
		}
		out.Servers = append(out.Servers, admin.DirectServer{ID: s.ID, Label: s.Label, URL: s.URL,
			Issuer: c.Issuer.URL, Resource: s.Resource, Scope: s.Scope, CAFile: ca})
	}
	return out, nil
}

func diagError(parser *hclparse.Parser, diags hcl.Diagnostics) error {
	var b strings.Builder
	wr := hcl.NewDiagnosticTextWriter(&b, parser.Files(), 0, false)
	_ = wr.WriteDiagnostics(diags)
	return errors.New(strings.TrimSpace(b.String()))
}
