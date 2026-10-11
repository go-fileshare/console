// SPDX-License-Identifier: BSD-3-Clause

// Package admintest is a stand-in for go-fileshare/portal and the fileshare
// servers behind it, for tests: in one process, with the portal's rules where
// the console depends on them.
//
//   - every call must carry Portal-Request: 1, or it is refused (403);
//   - a session holds tokens only for the servers its last refresh named,
//     and a call to another answers 401 with Portal-Refresh: <server>;
//   - refreshes are counted, and two that overlap are reported, because a
//     provider that rotates refresh tokens would end the login there.
//
// Behind it each server is a real Connect server (connect-go, the same
// generated handler interface fileshare implements) over a map of shares.
package admintest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	v1 "github.com/go-fileshare/console/internal/adminpb"
	adminv1connect "github.com/go-fileshare/console/internal/adminpb/adminpbconnect"
)

// Portal is the fake portal.
type Portal struct {
	*httptest.Server

	mu       sync.Mutex
	loggedIn bool
	tokens   map[string]time.Time // server -> token expiry
	servers  map[string]*Server
	order    []string

	Refreshes   atomic.Int64
	Overlapping atomic.Int64 // refreshes that began while another ran
	inRefresh   atomic.Bool
	// Delay makes each refresh take this long, so that overlaps can happen.
	Delay time.Duration
	// MaxTokens, when > 0, refuses a refresh for more servers (413), as the
	// portal does past its cookie budget.
	MaxTokens int
	// NoHeader counts calls that came without Portal-Request.
	NoHeader atomic.Int64
}

// Server is one fake fileshare server.
type Server struct {
	adminv1connect.UnimplementedAdminServiceHandler
	ID, Label string
	Reachable bool

	mu     sync.Mutex
	shares map[string]*v1.Share
	h      http.Handler
	// Calls counts the calls that reached this server.
	Calls atomic.Int64
	// Fail, when set, is answered to every call.
	Fail error
}

// New starts a fake portal with the given servers, logged in or not.
func New(loggedIn bool, servers ...*Server) *Portal {
	p := &Portal{loggedIn: loggedIn, tokens: map[string]time.Time{}, servers: map[string]*Server{}}
	for _, s := range servers {
		s.h = s.Handler()
		p.servers[s.ID] = s
		p.order = append(p.order, s.ID)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /session", p.session)
	mux.HandleFunc("POST /session/refresh", p.refresh)
	mux.HandleFunc("POST /logout", p.logout)
	mux.HandleFunc("POST /api/{server}/{rpc...}", p.api)
	p.Server = httptest.NewServer(p.guard(mux))
	return p
}

// NewServer is a fake fileshare server with these shares.
func NewServer(id, label string, shares ...*v1.Share) *Server {
	s := &Server{ID: id, Label: label, Reachable: true, shares: map[string]*v1.Share{}}
	for _, sh := range shares {
		s.shares[sh.GetName()] = sh
	}
	return s
}

// Share is a directory share with one grant, enabled.
func Share(name, dir string, subject string, write bool) *v1.Share {
	acc := v1.Access_ACCESS_READ
	if write {
		acc = v1.Access_ACCESS_WRITE
	}
	return &v1.Share{Name: name, Source: &v1.Share_Directory{Directory: dir}, Enabled: true,
		Origin: v1.Origin_ORIGIN_API, Grants: []*v1.Grant{{Subject: &v1.Subject{Kind: &v1.Subject_User{User: subject}}, Access: acc}}}
}

func (p *Portal) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Portal-Request") != "1" {
			p.NoHeader.Add(1)
			w.WriteHeader(http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// Expire drops every token, as time passing would.
func (p *Portal) Expire() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens = map[string]time.Time{}
}

// Logout ends the session, as an expired login would.
func (p *Portal) Logout() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.loggedIn = false
	p.tokens = map[string]time.Time{}
}

// Tokens is the servers the session holds a token for.
func (p *Portal) Tokens() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for s := range p.tokens {
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func (p *Portal) session(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loggedIn {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	type sv struct {
		ID        string     `json:"id"`
		Label     string     `json:"label,omitempty"`
		Reachable bool       `json:"reachable"`
		Token     *time.Time `json:"token_expires,omitempty"`
	}
	out := map[string]any{"issuer": "https://login.example.org", "subject": "alice-sub", "name": "alice",
		"expires": time.Now().Add(time.Hour)}
	var list []sv
	for _, id := range p.order {
		s := p.servers[id]
		v := sv{ID: id, Label: s.Label, Reachable: s.Reachable}
		if e, ok := p.tokens[id]; ok {
			v.Token = &e
		}
		list = append(list, v)
	}
	out["servers"] = list
	_ = json.NewEncoder(w).Encode(out)
}

func (p *Portal) refresh(w http.ResponseWriter, r *http.Request) {
	if !p.inRefresh.CompareAndSwap(false, true) {
		p.Overlapping.Add(1)
	} else {
		defer p.inRefresh.Store(false)
	}
	p.Refreshes.Add(1)
	time.Sleep(p.Delay)
	var req struct {
		Servers []string `json:"servers"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.loggedIn {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if p.MaxTokens > 0 && len(req.Servers) > p.MaxTokens {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	p.tokens = map[string]time.Time{}
	for _, s := range req.Servers {
		if sv, ok := p.servers[s]; !ok || !sv.Reachable {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		p.tokens[s] = time.Now().Add(5 * time.Minute)
	}
	_, _ = w.Write([]byte("{}"))
}

func (p *Portal) logout(w http.ResponseWriter, r *http.Request) {
	p.Logout()
	_, _ = w.Write([]byte(`{"end_session":"https://login.example.org/logout"}`))
}

func (p *Portal) api(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("server")
	p.mu.Lock()
	s, ok := p.servers[id]
	_, has := p.tokens[id]
	logged := p.loggedIn
	p.mu.Unlock()
	switch {
	case !ok:
		w.WriteHeader(http.StatusNotFound)
		return
	case !logged:
		w.WriteHeader(http.StatusUnauthorized)
		return
	case !has:
		w.Header().Set("Portal-Refresh", id)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	s.Calls.Add(1)
	r2 := r.Clone(r.Context())
	r2.URL.Path = "/" + r.PathValue("rpc")
	s.h.ServeHTTP(w, r2)
}

// --- the fake fileshare server ------------------------------------------------

func (s *Server) list() []*v1.Share {
	var names []string
	for n := range s.shares {
		names = append(names, n)
	}
	slices.Sort(names)
	var out []*v1.Share
	for _, n := range names {
		out = append(out, s.shares[n])
	}
	return out
}

func (s *Server) GetServerInfo(context.Context, *v1.GetServerInfoRequest) (*v1.GetServerInfoResponse, error) {
	if s.Fail != nil {
		return nil, s.Fail
	}
	return &v1.GetServerInfoResponse{Info: &v1.ServerInfo{Name: strings.ToUpper(s.ID), Version: "v0.28.1"}}, nil
}

func (s *Server) ListShares(context.Context, *v1.ListSharesRequest) (*v1.ListSharesResponse, error) {
	if s.Fail != nil {
		return nil, s.Fail
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &v1.ListSharesResponse{Shares: s.list()}, nil
}

func (s *Server) CreateShare(_ context.Context, r *v1.CreateShareRequest) (*v1.CreateShareResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.shares[r.GetName()]; ok {
		return nil, connect.NewError(connect.CodeAlreadyExists, "share "+r.GetName()+" exists")
	}
	if len(r.GetGrants()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, "a share needs a grant")
	}
	sh := &v1.Share{Name: r.GetName(), Source: &v1.Share_Directory{Directory: r.GetDirectory()},
		Grants: r.GetGrants(), Enabled: !r.GetDisabled(), Origin: v1.Origin_ORIGIN_API}
	s.shares[sh.Name] = sh
	return &v1.CreateShareResponse{Share: sh}, nil
}

func (s *Server) DeleteShare(_ context.Context, r *v1.DeleteShareRequest) (*v1.DeleteShareResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.shares[r.GetName()]; !ok {
		return nil, connect.NewError(connect.CodeNotFound, "no share "+r.GetName())
	}
	delete(s.shares, r.GetName())
	return &v1.DeleteShareResponse{}, nil
}

func (s *Server) DisableShare(_ context.Context, r *v1.DisableShareRequest) (*v1.DisableShareResponse, error) {
	return s.setEnabled(r.GetName(), false)
}

func (s *Server) EnableShare(_ context.Context, r *v1.EnableShareRequest) (*v1.EnableShareResponse, error) {
	resp, err := s.setEnabled(r.GetName(), true)
	if err != nil {
		return nil, err
	}
	return &v1.EnableShareResponse{Share: resp.Share}, nil
}

func (s *Server) setEnabled(name string, on bool) (*v1.DisableShareResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shares[name]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "no share "+name)
	}
	sh.Enabled = on
	return &v1.DisableShareResponse{Share: sh}, nil
}

func (s *Server) Grant(_ context.Context, r *v1.GrantRequest) (*v1.GrantResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shares[r.GetShare()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "no share "+r.GetShare())
	}
	sh.Grants = append(sh.Grants, r.GetGrant())
	return &v1.GrantResponse{Share: sh}, nil
}

func (s *Server) Revoke(_ context.Context, r *v1.RevokeRequest) (*v1.RevokeResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sh, ok := s.shares[r.GetShare()]
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound, "no share "+r.GetShare())
	}
	if len(sh.Grants) == 1 {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "the last grant: delete the share instead")
	}
	sh.Grants = slices.DeleteFunc(sh.Grants, func(g *v1.Grant) bool {
		return g.GetSubject().String() == r.GetSubject().String()
	})
	return &v1.RevokeResponse{Share: sh}, nil
}

// Names is the shares on s, sorted.
func (s *Server) Names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, sh := range s.list() {
		out = append(out, sh.GetName())
	}
	return out
}

// Handler is the server's admin API as a Connect handler, to be served on its
// own -- as a native console reaches a server, without a portal.
func (s *Server) Handler() http.Handler {
	if s.shares == nil {
		s.shares = map[string]*v1.Share{}
	}
	srv := connect.NewServer()
	adminv1connect.RegisterAdminServiceHandler(srv, s)
	mux := http.NewServeMux()
	connecthttp.Mount(mux, srv)
	return mux
}
