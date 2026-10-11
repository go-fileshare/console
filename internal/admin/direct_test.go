// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

package admin

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/go-authn/oidc"

	v1 "github.com/go-fileshare/console/internal/adminpb"

	"github.com/go-fileshare/console/internal/admin/admintest"
)

// direct is a native console's world: a provider, two servers that verify
// their token as fileshare does (go-authn/oidc, their own audience), and a
// browser that logs in when asked.
type direct struct {
	idp    *testIdP
	a, b   *admintest.Server
	d      *Direct
	opened atomic.Int64
	// refused counts the calls a server turned away for their token.
	refused atomic.Int64
}

func newDirect(t *testing.T) *direct {
	t.Helper()
	w := &direct{idp: newTestIdP(t)}
	w.idp.public = true
	w.a = admintest.NewServer("a", "Paris", admintest.Share("photos", "/srv/photos", "alice", true))
	w.b = admintest.NewServer("b", "Lyon")
	serve := func(s *admintest.Server, aud string) string {
		v, err := oidc.New(context.Background(), oidc.Config{Issuer: w.idp.issuer, Audience: aud})
		if err != nil {
			t.Fatal(err)
		}
		h := s.Handler()
		srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			raw, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if _, err := v.Verify(r.Context(), raw); err != nil {
				w.refused.Add(1)
				rw.WriteHeader(http.StatusUnauthorized)
				return
			}
			h.ServeHTTP(rw, r)
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	w.idp.resources = []string{"https://a.example/", "https://b.example/"}
	cfg := DirectConfig{Issuer: w.idp.issuer, ClientID: w.idp.clientID, Servers: []DirectServer{
		{ID: "a", Label: "Paris", URL: serve(w.a, "https://a.example/"), Issuer: w.idp.issuer, Resource: "https://a.example/"},
		{ID: "b", Label: "Lyon", URL: serve(w.b, "https://b.example/"), Issuer: w.idp.issuer, Resource: "https://b.example/"},
		{ID: "c", URL: "https://c.example", Issuer: "https://other.example", Resource: "https://c.example/"},
	}}
	d, err := NewDirect(cfg, w.browser(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	w.d = d
	return w
}

// browser opens the URL as the system browser would: the provider's login,
// then its redirect to the console's loopback listener.
func (w *direct) browser(t *testing.T) func(string) error {
	return func(auth string) error {
		w.opened.Add(1)
		go func() {
			nc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := nc.Get(auth)
			if err != nil {
				t.Error(err)
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
}

func TestDirectLoginAndCalls(t *testing.T) {
	w := newDirect(t)
	ctx := context.Background()
	s, err := w.d.Session(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Who() != "alice" || len(s.Servers) != 3 || !s.Servers[0].Reachable || s.Servers[2].Reachable {
		t.Fatalf("%+v", s)
	}
	// Logged in once: a second Session does not open the browser again.
	if _, err := w.d.Session(ctx); err != nil || w.opened.Load() != 1 {
		t.Fatalf("%v, browser opened %d times", err, w.opened.Load())
	}
	res, err := w.d.Client("a").ListShares(ctx, &v1.ListSharesRequest{})
	if err != nil || len(res.GetShares()) != 1 {
		t.Fatalf("%v %v", err, res)
	}
	if _, err := w.d.Client("b").ListShares(ctx, &v1.ListSharesRequest{}); err != nil {
		t.Fatal(err)
	}
	// Each server got a token for itself: none was refused.
	if w.refused.Load() != 0 {
		t.Fatalf("%d calls refused for their token", w.refused.Load())
	}
	// The token is kept: no refresh for the next call.
	before := w.idp.refreshN
	if _, err := w.d.Client("a").ListShares(ctx, &v1.ListSharesRequest{}); err != nil || w.idp.refreshN != before {
		t.Fatalf("%v; %d refreshes", err, w.idp.refreshN-before)
	}
	if w.d.LoginURL("/x") != LoginAgain {
		t.Fatal("LoginURL")
	}
}

// Many calls at once, to two servers: one refresh at a time, so a rotating
// provider never sees a refresh token twice and the login survives.
func TestDirectConcurrentCalls(t *testing.T) {
	w := newDirect(t)
	ctx := context.Background()
	if _, err := w.d.Session(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := range 20 {
		wg.Go(func() {
			_, err := w.d.Client([]string{"a", "b"}[i%2]).ListShares(ctx, &v1.ListSharesRequest{})
			errs <- err
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if w.idp.refreshN != 2 {
		t.Fatalf("%d refreshes for two servers", w.idp.refreshN)
	}
}

// A server that refuses the token it holds (revoked, say): one refresh, one
// more try.
func TestDirectRetriesOnceAfterA401(t *testing.T) {
	w := newDirect(t)
	ctx := context.Background()
	if _, err := w.d.Session(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.d.Client("a").ListShares(ctx, &v1.ListSharesRequest{}); err != nil {
		t.Fatal(err)
	}
	// Spoil the kept token.
	w.d.mu.Lock()
	tok := w.d.tokens["a"]
	tok.AccessToken = "spoiled"
	w.d.tokens["a"] = tok
	w.d.mu.Unlock()
	if _, err := w.d.Client("a").CreateShare(ctx, &v1.CreateShareRequest{Name: "music",
		Source: &v1.CreateShareRequest_Directory{Directory: "/srv/music"},
		Grants: []*v1.Grant{{Subject: &v1.Subject{Kind: &v1.Subject_User{User: "bob"}}}}}); err != nil {
		t.Fatal(err)
	}
	if w.refused.Load() != 1 || !strings.Contains(strings.Join(w.a.Names(), ","), "music") {
		t.Fatalf("%d refused, shares %v", w.refused.Load(), w.a.Names())
	}
}

// The grant ended at the provider: the console is told to log in again, and
// the next Session does.
func TestDirectAnEndedGrant(t *testing.T) {
	w := newDirect(t)
	ctx := context.Background()
	if _, err := w.d.Session(ctx); err != nil {
		t.Fatal(err)
	}
	w.idp.mu.Lock()
	for fam := range w.idp.live {
		w.idp.revoked[w.idp.live[fam]] = true
	}
	w.idp.mu.Unlock()
	_, err := w.d.Client("a").ListShares(ctx, &v1.ListSharesRequest{})
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("%v", err)
	}
	if _, err := w.d.Session(ctx); err != nil || w.opened.Load() != 2 {
		t.Fatalf("%v; browser opened %d times", err, w.opened.Load())
	}
	if _, err := w.d.Client("a").ListShares(ctx, &v1.ListSharesRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestDirectLogout(t *testing.T) {
	w := newDirect(t)
	ctx := context.Background()
	if _, err := w.d.Session(ctx); err != nil {
		t.Fatal(err)
	}
	end, err := w.d.Logout(ctx)
	if err != nil || !strings.HasPrefix(end, w.idp.issuer+"/logout") {
		t.Fatalf("%q %v", end, err)
	}
	if _, err := w.d.Client("a").ListShares(ctx, &v1.ListSharesRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal(err)
	}
	if end, _ := w.d.Logout(ctx); end != "" {
		t.Fatal("an end-session URL with nobody logged in")
	}
}

func TestDirectRefusals(t *testing.T) {
	for name, cfg := range map[string]DirectConfig{
		"no issuer":   {ClientID: "c", Servers: []DirectServer{{ID: "a", Resource: "r"}}},
		"no client":   {Issuer: "https://i", Servers: []DirectServer{{ID: "a", Resource: "r"}}},
		"no server":   {Issuer: "https://i", ClientID: "c"},
		"no audience": {Issuer: "https://i", ClientID: "c", Servers: []DirectServer{{ID: "a"}}},
	} {
		if _, err := NewDirect(cfg, nil, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	w := newDirect(t)
	ctx := context.Background()
	if _, err := w.d.Client("nowhere").ListShares(ctx, &v1.ListSharesRequest{}); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatal(err)
	}
	// A server whose CA file does not load is unreachable, and says why.
	w.d.cfg.Servers = append(w.d.cfg.Servers, DirectServer{ID: "x", URL: "https://x", Resource: "r", CAFile: "/nonexistent"})
	if _, err := w.d.Client("x").ListShares(ctx, &v1.ListSharesRequest{}); err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Fatal(err)
	}
	// Not logged in: unauthenticated, before any server is called.
	fresh := newDirect(t)
	if _, err := fresh.d.Client("a").ListShares(ctx, &v1.ListSharesRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatal(err)
	}
	// No browser: the login fails, and says so.
	nob := newDirect(t)
	nob.d.open = func(string) error { return errors.New("no display") }
	if _, err := nob.d.Session(ctx); err == nil {
		t.Fatal("logged in with no browser")
	}
	// No refresh token from the provider: refused, with the reason.
	norefresh := newDirect(t)
	norefresh.idp.noRefresh = true
	if _, err := norefresh.d.Session(ctx); err == nil || !strings.Contains(err.Error(), "offline_access") {
		t.Fatal(err)
	}
}

// A server whose certificate only its own CA vouches for -- an internal one
// -- is reached through ca_file, and refused without it.
func TestDirectServerCA(t *testing.T) {
	w := newDirect(t)
	tlsSrv := httptest.NewTLSServer(w.a.Handler())
	defer tlsSrv.Close()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	_ = os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: tlsSrv.Certificate().Raw}), 0o644)
	empty := filepath.Join(dir, "empty.pem")
	_ = os.WriteFile(empty, []byte("nothing"), 0o644)
	w.d.cfg.Servers = append(w.d.cfg.Servers,
		DirectServer{ID: "trusted", URL: tlsSrv.URL, Issuer: w.idp.issuer, Resource: "https://a.example/", CAFile: ca},
		DirectServer{ID: "untrusted", URL: tlsSrv.URL, Issuer: w.idp.issuer, Resource: "https://a.example/"},
		DirectServer{ID: "empty", URL: tlsSrv.URL, Issuer: w.idp.issuer, Resource: "https://a.example/", CAFile: empty})
	ctx := context.Background()
	if _, err := w.d.Session(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := w.d.Client("trusted").ListShares(ctx, &v1.ListSharesRequest{}); err != nil {
		t.Fatalf("with its CA: %v", err)
	}
	if _, err := w.d.Client("untrusted").ListShares(ctx, &v1.ListSharesRequest{}); err == nil {
		t.Fatal("a certificate nobody vouches for was accepted")
	}
	if _, err := w.d.Client("empty").ListShares(ctx, &v1.ListSharesRequest{}); err == nil || !strings.Contains(err.Error(), "holds no certificate") {
		t.Fatal(err)
	}
}

// A server that redirects is not followed. Go's client keeps Authorization
// on a redirect to the same host -- or a subdomain -- whatever the scheme or
// port, so following one would hand the token to whatever answers there. The
// listener on the other end records what reaches it.
func TestDirectFollowsNoRedirect(t *testing.T) {
	var leaked atomic.Value
	other := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		leaked.Store(r.Header.Get("Authorization"))
	}))
	defer other.Close()
	w := newDirect(t)
	ctx := context.Background()
	if _, err := w.d.Session(ctx); err != nil {
		t.Fatal(err)
	}
	hop := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		http.Redirect(rw, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer hop.Close()
	hc, err := serverClient(w.d.cfg.Servers[0])
	if err != nil {
		t.Fatal(err)
	}
	h := &directHTTP{d: w.d, sv: w.d.cfg.Servers[0], hc: hc}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, hop.URL+"/fileshare.admin.v1.AdminService/ListShares", strings.NewReader("{}"))
	res, err := h.Do(req)
	if err == nil {
		res.Body.Close()
	}
	if v, _ := leaked.Load().(string); v != "" {
		t.Fatalf("the token followed a redirect: %.20s...", v)
	}
	if err != nil || res.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("want the redirect answered as it is: %v %v", err, res)
	}
}
