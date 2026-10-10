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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

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
var ErrNotLoggedIn = errors.New("not logged in")

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

// Portal is a Backend through go-fileshare/portal.
type Portal struct {
	base string // the portal's origin, no trailing slash
	hc   *http.Client
	// serialize runs the refresh with every other one, across the tabs of
	// this browser where it can (the Web Locks API, in the js build), so that
	// the portal never sees two refreshes at once: a provider that rotates
	// refresh tokens revokes the whole login when one is spent twice.
	serialize func(func() error) error

	mu      sync.Mutex
	clients map[string]adminv1connect.AdminServiceClient
	inUse   []string // servers recently called, most recent first
	now     func() time.Time
}

// MaxInUse is how many servers' tokens one refresh asks for: the ones most
// recently used. The portal keeps its session in cookies, and a browser sends
// those on one header line that a load balancer bounds (8 KiB at nginx's
// default); five tokens of about 1 KB fit. Calling a sixth server makes the
// least recently used one ask again when it is next called.
const MaxInUse = 5

// NewPortal is a Backend through the portal at base (an origin, such as
// "https://files.example.org", or "" for the page's own). serialize may be
// nil: refreshes are then serialized within this process only.
func NewPortal(base string, hc *http.Client, serialize func(func() error) error) *Portal {
	if hc == nil {
		hc = http.DefaultClient
	}
	p := &Portal{base: strings.TrimSuffix(base, "/"), hc: hc,
		clients: map[string]adminv1connect.AdminServiceClient{}, now: time.Now}
	if serialize == nil {
		var mu sync.Mutex
		serialize = func(fn func() error) error {
			mu.Lock()
			defer mu.Unlock()
			return fn()
		}
	}
	p.serialize = serialize
	return p
}

// requestHeader is the static header the portal requires of every call
// its pages make (its CSRF defence).
const requestHeader = "Portal-Request"

// sessionView is the portal's GET /session answer.
type sessionView struct {
	Issuer  string    `json:"issuer"`
	Subject string    `json:"subject"`
	Name    string    `json:"name"`
	Expires time.Time `json:"expires"`
	Servers []struct {
		ID        string     `json:"id"`
		Label     string     `json:"label"`
		Reachable bool       `json:"reachable"`
		Token     *time.Time `json:"token_expires"`
	} `json:"servers"`
}

func (p *Portal) get(ctx context.Context) (*sessionView, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.base+"/session", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set(requestHeader, "1")
	res, err := p.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized {
		return nil, ErrNotLoggedIn
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("portal: %s", res.Status)
	}
	var v sessionView
	if err := json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&v); err != nil {
		return nil, fmt.Errorf("portal: %w", err)
	}
	return &v, nil
}

// Session is who is logged in.
func (p *Portal) Session(ctx context.Context) (*Session, error) {
	v, err := p.get(ctx)
	if err != nil {
		return nil, err
	}
	s := &Session{Issuer: v.Issuer, Subject: v.Subject, Name: v.Name, Expires: v.Expires}
	for _, sv := range v.Servers {
		s.Servers = append(s.Servers, Server{ID: sv.ID, Label: sv.Label, Reachable: sv.Reachable})
	}
	return s, nil
}

// LoginURL is the portal's login, coming back to returnTo (a path).
func (p *Portal) LoginURL(returnTo string) string {
	return p.base + "/login?return=" + url.QueryEscape(returnTo)
}

// Logout ends the session.
func (p *Portal) Logout(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/logout", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set(requestHeader, "1")
	res, err := p.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var out struct {
		EndSession string `json:"end_session"`
	}
	_ = json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&out)
	return out.EndSession, nil
}

// Client is the admin API of server, through the portal.
func (p *Portal) Client(server string) adminv1connect.AdminServiceClient {
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.clients[server]; ok {
		return c
	}
	hc := &serverHTTP{p: p, server: server}
	c := adminv1connect.NewAdminServiceClient(connect.NewClient(
		connecthttp.NewTransport(hc, p.base+"/api/"+server)))
	p.clients[server] = c
	return c
}

// used records that server was just called, and returns the servers whose
// tokens a refresh should ask for: it, and the others most recently used.
func (p *Portal) used(server string) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.inUse = slices.DeleteFunc(p.inUse, func(s string) bool { return s == server })
	p.inUse = slices.Insert(p.inUse, 0, server)
	if len(p.inUse) > MaxInUse {
		p.inUse = p.inUse[:MaxInUse]
	}
	return slices.Clone(p.inUse)
}

// refresh asks the portal for tokens for servers, unless -- another tab, or
// another call of this one, having refreshed meanwhile -- the session already
// holds a current token for need.
func (p *Portal) refresh(ctx context.Context, need string, servers []string) error {
	return p.serialize(func() error {
		v, err := p.get(ctx)
		if err != nil {
			return err
		}
		for _, sv := range v.Servers {
			if sv.ID == need && sv.Token != nil && sv.Token.After(p.now().Add(30*time.Second)) {
				return nil
			}
		}
		err = p.postRefresh(ctx, servers)
		var tooMany *tooLarge
		if errors.As(err, &tooMany) && len(servers) > 1 {
			// The tokens of all these do not fit: ask for the one needed.
			return p.postRefresh(ctx, []string{need})
		}
		return err
	})
}

type tooLarge struct{}

func (*tooLarge) Error() string { return "portal: too many servers at once" }

func (p *Portal) postRefresh(ctx context.Context, servers []string) error {
	body, _ := json.Marshal(map[string][]string{"servers": servers})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.base+"/session/refresh", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set(requestHeader, "1")
	req.Header.Set("Content-Type", "application/json")
	res, err := p.hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))
	switch res.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return ErrNotLoggedIn
	case http.StatusRequestEntityTooLarge:
		return &tooLarge{}
	}
	return fmt.Errorf("portal: refresh: %s", res.Status)
}

// serverHTTP is the HTTP client of one server's Connect transport: it adds
// the portal's header, and when the portal answers that the server's token
// is missing or expiring (401, Portal-Refresh), refreshes and sends the call
// once more. A call is at most 1 MiB, so it is kept to be sent again.
type serverHTTP struct {
	p      *Portal
	server string
}

const maxCall = 1 << 20

func (h *serverHTTP) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(io.LimitReader(req.Body, maxCall+1))
		req.Body.Close()
		if err != nil {
			return nil, err
		}
		if len(b) > maxCall {
			return nil, connect.NewError(connect.CodeResourceExhausted, "a call is at most 1 MiB")
		}
		body = b
	}
	servers := h.p.used(h.server)
	send := func() (*http.Response, error) {
		r := req.Clone(req.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set(requestHeader, "1")
		return h.p.hc.Do(r)
	}
	res, err := send()
	if err != nil || res.StatusCode != http.StatusUnauthorized || res.Header.Get("Portal-Refresh") == "" {
		return res, err
	}
	res.Body.Close()
	if err := h.p.refresh(req.Context(), h.server, servers); err != nil {
		if errors.Is(err, ErrNotLoggedIn) {
			return nil, connect.NewError(connect.CodeUnauthenticated, ErrNotLoggedIn.Error())
		}
		return nil, connect.NewError(connect.CodeUnavailable, err.Error())
	}
	return send()
}
