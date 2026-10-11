// SPDX-License-Identifier: BSD-3-Clause

package e2e

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/go-fileshare/console/internal/admin"
	v1 "github.com/go-fileshare/console/internal/adminpb"
)

// The native console's backend -- a public client logging in through a
// loopback listener (go-authn/rp, RFC 8252) and calling each server with its
// own token -- against two REAL fileshare servers. No window: what is
// tested is that real fileshare takes what the native console sends.
func TestE2ENativeAgainstRealServers(t *testing.T) {
	fsBin := os.Getenv("FILESHARE")
	if fsBin == "" {
		t.Skip("FILESHARE names the fileshare binary to test against")
	}
	dir := t.TempDir()
	cert, key := selfSigned(t, dir)
	idp := newTestIdP(t)
	idp.public = true
	a := startFileshare(t, fsBin, idp, "A", "https://fs-a.test/", cert, key)
	b := startFileshare(t, fsBin, idp, "B", "https://fs-b.test/", cert, key)
	idp.resources = []string{a.audience, b.audience}

	browser := func(auth string) error {
		go func() {
			nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := nc.Get(auth)
			if err != nil {
				return
			}
			res.Body.Close()
			back, _ := url.Parse(res.Header.Get("Location"))
			if res, err := http.Get(back.String()); err == nil {
				res.Body.Close()
			}
		}()
		return nil
	}
	d, err := admin.NewDirect(admin.DirectConfig{Issuer: idp.issuer, ClientID: idp.clientID, Servers: []admin.DirectServer{
		{ID: "a", URL: "https://" + a.addr, Issuer: idp.issuer, Resource: a.audience, CAFile: cert},
		{ID: "b", URL: "https://" + b.addr, Issuer: idp.issuer, Resource: b.audience, CAFile: cert},
	}}, browser, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := d.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Who() != "alice" {
		t.Fatalf("%+v", s)
	}
	if _, err := d.Client("b").ListShares(ctx, &v1.ListSharesRequest{}); err != nil {
		t.Fatalf("B: %v", err)
	}
	_, err = d.Client("a").CreateShare(ctx, &v1.CreateShareRequest{Name: "data",
		Source: &v1.CreateShareRequest_Directory{Directory: a.data},
		Grants: []*v1.Grant{{Subject: &v1.Subject{Kind: &v1.Subject_User{User: "alice"}}, Access: v1.Access_ACCESS_WRITE}}})
	if err != nil {
		t.Fatalf("CreateShare on A: %v", err)
	}
	list, err := d.Client("a").ListShares(ctx, &v1.ListSharesRequest{})
	if err != nil || len(list.GetShares()) != 1 || list.GetShares()[0].GetName() != "data" {
		t.Fatalf("%v %v", err, list)
	}
	if !strings.Contains(a.out.String(), "oidc="+idp.issuer+" alice-sub") {
		t.Fatalf("A's audit does not name the caller:\n%s", a.out)
	}
	if strings.Contains(b.out.String(), "oidc=") {
		t.Fatalf("B audited a change it never received:\n%s", b.out)
	}
}
