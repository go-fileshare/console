// SPDX-License-Identifier: BSD-3-Clause

package admin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect/v2"

	v1 "github.com/go-fileshare/console/internal/adminpb"

	"github.com/go-fileshare/console/internal/admin/admintest"
)

func TestSessionAndLogin(t *testing.T) {
	fp := admintest.New(true, admintest.NewServer("a", "Paris"), &admintest.Server{ID: "b"})
	defer fp.Close()
	p := NewPortal(fp.URL+"/", nil, nil)
	s, err := p.Session(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Who() != "alice" || len(s.Servers) != 2 || s.Servers[0].Name() != "Paris" || s.Servers[1].Name() != "b" || s.Servers[1].Reachable {
		t.Fatalf("%+v", s)
	}
	if got := p.LoginURL("/console/?x=1"); got != fp.URL+"/login?return=%2Fconsole%2F%3Fx%3D1" {
		t.Fatalf("login URL %q", got)
	}
	if (&Session{Subject: "s"}).Who() != "s" {
		t.Fatal("Who without a name")
	}
	fp.Logout()
	if _, err := p.Session(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatalf("after logout: %v", err)
	}
}

// The first call has no token: the portal says to refresh, the client does,
// once, and the call goes through -- carrying the static header every time.
func TestACallRefreshesOnceThenGoesThrough(t *testing.T) {
	a := admintest.NewServer("a", "", admintest.Share("photos", "/srv/photos", "alice", true))
	fp := admintest.New(true, a)
	defer fp.Close()
	p := NewPortal(fp.URL, nil, nil)
	res, err := p.Client("a").ListShares(context.Background(), &v1.ListSharesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.GetShares()) != 1 || res.GetShares()[0].GetName() != "photos" {
		t.Fatal(res)
	}
	if fp.Refreshes.Load() != 1 || fp.NoHeader.Load() != 0 {
		t.Fatalf("%d refreshes, %d calls without the header", fp.Refreshes.Load(), fp.NoHeader.Load())
	}
	// The token is current: no refresh for the next call.
	if _, err := p.Client("a").ListShares(context.Background(), &v1.ListSharesRequest{}); err != nil || fp.Refreshes.Load() != 1 {
		t.Fatalf("second call: %v, %d refreshes", err, fp.Refreshes.Load())
	}
	// A call with a body is sent again whole after a refresh.
	fp.Expire()
	if _, err := p.Client("a").CreateShare(context.Background(), &v1.CreateShareRequest{Name: "music",
		Source: &v1.CreateShareRequest_Directory{Directory: "/srv/music"},
		Grants: []*v1.Grant{{Subject: &v1.Subject{Kind: &v1.Subject_User{User: "bob"}}, Access: v1.Access_ACCESS_READ}}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(a.Names(), ","); got != "music,photos" {
		t.Fatal(got)
	}
}

// Ten calls at once to a server with no token make ONE refresh, never two at
// a time: a provider that rotates refresh tokens ends the login otherwise.
func TestConcurrentCallsShareOneRefresh(t *testing.T) {
	fp := admintest.New(true, admintest.NewServer("a", ""))
	fp.Delay = 50 * time.Millisecond
	defer fp.Close()
	p := NewPortal(fp.URL, nil, nil)
	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Go(func() {
			_, err := p.Client("a").ListShares(context.Background(), &v1.ListSharesRequest{})
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
	if fp.Overlapping.Load() != 0 {
		t.Fatalf("%d refreshes overlapped", fp.Overlapping.Load())
	}
	if fp.Refreshes.Load() != 1 {
		t.Fatalf("%d refreshes for one missing token", fp.Refreshes.Load())
	}
}

// A refresh asks for the servers in use, so that calling B does not drop A's
// token -- the portal keeps only what the last refresh named.
func TestARefreshKeepsTheServersInUse(t *testing.T) {
	fp := admintest.New(true, admintest.NewServer("a", ""), admintest.NewServer("b", ""))
	defer fp.Close()
	p := NewPortal(fp.URL, nil, nil)
	ctx := context.Background()
	for _, s := range []string{"a", "b", "a", "b"} {
		if _, err := p.Client(s).ListShares(ctx, &v1.ListSharesRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	if fp.Refreshes.Load() != 2 {
		t.Fatalf("%d refreshes for two servers used alternately", fp.Refreshes.Load())
	}
	if got := fmt.Sprint(fp.Tokens()); got != "[a b]" {
		t.Fatalf("tokens %s", got)
	}
}

// Past the portal's cookie budget, the refresh falls back to the one server
// the call needs.
func TestTooManyServersFallsBackToOne(t *testing.T) {
	var servers []*admintest.Server
	for i := range MaxInUse + 2 {
		servers = append(servers, admintest.NewServer(fmt.Sprint("s", i), ""))
	}
	fp := admintest.New(true, servers...)
	fp.MaxTokens = 2
	defer fp.Close()
	p := NewPortal(fp.URL, nil, nil)
	for i := range MaxInUse + 2 {
		if _, err := p.Client(fmt.Sprint("s", i)).ListShares(context.Background(), &v1.ListSharesRequest{}); err != nil {
			t.Fatalf("s%d: %v", i, err)
		}
	}
}

// A login that has ended: the call fails as unauthenticated, which the
// console reads as "log in again".
func TestAnEndedLoginIsUnauthenticated(t *testing.T) {
	fp := admintest.New(true, admintest.NewServer("a", ""))
	defer fp.Close()
	p := NewPortal(fp.URL, nil, nil)
	fp.Logout()
	_, err := p.Client("a").ListShares(context.Background(), &v1.ListSharesRequest{})
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("%v (%v)", err, connect.CodeOf(err))
	}
	// Logged in, but the refresh fails otherwise: unavailable.
	fp2 := admintest.New(true, &admintest.Server{ID: "b"}) // not reachable: the refresh is refused
	defer fp2.Close()
	_, err = NewPortal(fp2.URL, nil, nil).Client("b").ListShares(context.Background(), &v1.ListSharesRequest{})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("%v (%v)", err, connect.CodeOf(err))
	}
}

func TestLogout(t *testing.T) {
	fp := admintest.New(true, admintest.NewServer("a", ""))
	defer fp.Close()
	p := NewPortal(fp.URL, nil, nil)
	end, err := p.Logout(context.Background())
	if err != nil || end != "https://login.example.org/logout" {
		t.Fatalf("%q %v", end, err)
	}
	if _, err := p.Session(context.Background()); !errors.Is(err, ErrNotLoggedIn) {
		t.Fatal(err)
	}
}

func TestACallLargerThanAMebibyte(t *testing.T) {
	fp := admintest.New(true, admintest.NewServer("a", ""))
	defer fp.Close()
	p := NewPortal(fp.URL, nil, nil)
	_, err := p.Client("a").CreateShare(context.Background(), &v1.CreateShareRequest{Name: strings.Repeat("x", maxCall+1)})
	if connect.CodeOf(err) != connect.CodeResourceExhausted {
		t.Fatalf("%v", err)
	}
	if fp.Refreshes.Load() != 0 {
		t.Fatal("a call too large to send was sent")
	}
}

// A refresh the portal could not do (its provider down, say) is not the end
// of the login: Unavailable, not Unauthenticated. And a portal that does not
// answer is the transport's error, as it is.
func TestARefreshThatFailsIsUnavailable(t *testing.T) {
	fp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/session":
			_, _ = w.Write([]byte(`{}`))
		case "/session/refresh":
			w.WriteHeader(http.StatusBadGateway)
		default:
			w.Header().Set("Portal-Refresh", "1")
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer fp.Close()
	_, err := NewPortal(fp.URL, nil, nil).Client("a").ListShares(context.Background(), &v1.ListSharesRequest{})
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("%v: %v", connect.CodeOf(err), err)
	}
	_, err = NewPortal("http://127.0.0.1:1", nil, nil).Client("a").ListShares(context.Background(), &v1.ListSharesRequest{})
	if err == nil || connect.CodeOf(err) == connect.CodeUnauthenticated {
		t.Fatalf("nothing listening: %v", err)
	}
}
