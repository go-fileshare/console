// SPDX-License-Identifier: BSD-3-Clause

// Package admin is how the console reaches fileshare servers: their admin
// API (fileshare.admin.v1.AdminService, over Connect), and who is logged in.
//
// In a browser it goes through go-fileshare/portal, which holds the tokens:
// the console sends its calls to /api/<server>/..., with the session cookie
// the browser adds and the static header the portal requires, and never sees
// a token.
package admin

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/go-fileshare/portal/client"

	adminv1connect "github.com/go-fileshare/console/internal/adminpb/adminpbconnect"
)

// A Server is one fileshare server the console can manage.
type Server struct {
	ID    string
	Label string
	// Reachable: its tokens come from the identity provider this session
	// logged in with. A server behind another provider is listed, not
	// reachable.
	Reachable bool
}

// Name is how the console shows the server.
func (s Server) Name() string {
	if s.Label != "" {
		return s.Label
	}
	return s.ID
}

// A Session is who is logged in, and which servers they can reach.
type Session struct {
	Issuer, Subject, Name string
	Expires               time.Time
	Servers               []Server
}

// Who is how the console names the person.
func (s *Session) Who() string {
	if s.Name != "" {
		return s.Name
	}
	return s.Subject
}

// ErrNotLoggedIn is the answer when there is no session, or it has ended.
// It is the portal client's own, so that errors.Is recognises both.
var ErrNotLoggedIn = client.ErrNotLoggedIn

// A Backend is what the console needs.
type Backend interface {
	Session(ctx context.Context) (*Session, error)
	// Client is the admin API of one server.
	Client(server string) adminv1connect.AdminServiceClient
	// Logout ends the session, and returns where the browser should go
	// next to end it at the identity provider too ("" if nowhere).
	Logout(ctx context.Context) (string, error)
	// LoginURL is where to send a browser that has no session.
	LoginURL(returnTo string) string
}

// Portal is a Backend through go-fileshare/portal, by its client package
// (go-fileshare/portal/client): the same session, refresh and CSRF handling
// as the explorer's.
type Portal struct {
	c *client.Portal

	mu      sync.Mutex
	clients map[string]adminv1connect.AdminServiceClient
}

// MaxInUse is how many servers' tokens one refresh asks for: the ones most
// recently used (see client.MaxInUse).
const MaxInUse = client.MaxInUse

// NewPortal is a Backend through the portal at base (an origin, such as
// "https://files.example.org", or "" for the page's own). serialize may be
// nil: refreshes are then serialized within this process only; in a browser
// it is client.WebLocks, which serializes them across tabs.
func NewPortal(base string, hc *http.Client, serialize func(func() error) error) *Portal {
	return &Portal{c: client.New(base, hc, serialize), clients: map[string]adminv1connect.AdminServiceClient{}}
}

// Session is who is logged in, or ErrNotLoggedIn.
func (p *Portal) Session(ctx context.Context) (*Session, error) {
	cs, err := p.c.Session(ctx)
	if err != nil {
		return nil, err
	}
	s := &Session{Issuer: cs.Issuer, Subject: cs.Subject, Name: cs.Name, Expires: cs.Expires}
	for _, sv := range cs.Servers {
		s.Servers = append(s.Servers, Server{ID: sv.ID, Label: sv.Label, Reachable: sv.Reachable})
	}
	return s, nil
}

// LoginURL is the portal's login, coming back to returnTo.
func (p *Portal) LoginURL(returnTo string) string { return p.c.LoginURL(returnTo) }

// Logout ends the session at the portal.
func (p *Portal) Logout(ctx context.Context) (string, error) { return p.c.Logout(ctx) }

// maxCall bounds one call: it is kept to be sent again after a refresh.
const maxCall = 1 << 20

// Client is the admin API of server, through the portal.
func (p *Portal) Client(server string) adminv1connect.AdminServiceClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[server]; ok {
		return c
	}
	hc := connectErrors{p.c.Doer(server, maxCall)}
	c := adminv1connect.NewAdminServiceClient(connect.NewClient(
		connecthttp.NewTransport(hc, p.c.Base()+"/api/"+server)))
	p.clients[server] = c
	return c
}

// connectErrors gives the portal client's errors the Connect codes the
// console reads: an ended login is Unauthenticated (log in again), a call too
// large ResourceExhausted, a refresh the portal could not do Unavailable.
type connectErrors struct{ d *client.Doer }

func (h connectErrors) Do(req *http.Request) (*http.Response, error) {
	res, err := h.d.Do(req)
	var re *client.RefreshError
	switch {
	case err == nil:
		return res, nil
	case errors.Is(err, client.ErrNotLoggedIn):
		return nil, connect.NewError(connect.CodeUnauthenticated, err.Error())
	case errors.Is(err, client.ErrTooLarge):
		return nil, connect.NewError(connect.CodeResourceExhausted, "a call is at most 1 MiB")
	case errors.As(err, &re):
		return nil, connect.NewError(connect.CodeUnavailable, err.Error())
	}
	return nil, err
}
