// SPDX-License-Identifier: BSD-3-Clause

//go:build !(js && wasm)

// The native console's backend: a browser page goes through the portal
// instead, and never links any of this.

package admin

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"github.com/go-authn/rp"

	adminv1connect "github.com/go-fileshare/console/internal/adminpb/adminpbconnect"
)

// DirectConfig is what a native console needs to reach servers on its own,
// without a portal: where to log in, and the servers.
type DirectConfig struct {
	Issuer   string
	ClientID string // a PUBLIC client: no secret (RFC 8252 §8.5)
	Scopes   []string
	Servers  []DirectServer
}

// DirectServer is one fileshare server's admin API over HTTPS.
type DirectServer struct {
	ID, Label string
	URL       string // the admin block's web listener
	Issuer    string
	Resource  string // RFC 8707: the token's audience, the server's issuer audience
	Scope     string // for a provider that sets the audience from a scope
	CAFile    string // when its certificate is not publicly trusted
}

// Direct is a Backend that logs in itself -- through the system browser and a
// loopback listener (go-authn/rp, RFC 8252) -- and calls each server with a
// token addressed to it alone. The refresh token stays in this process's
// memory, and one refresh runs at a time: the provider rotates a public
// client's refresh tokens and ends the login when one is spent twice.
type Direct struct {
	cfg  DirectConfig
	open func(string) error
	hc   *http.Client // for the provider
	now  func() time.Time

	mu      sync.Mutex // the login, the refresh token and the tokens
	client  *rp.Client
	refresh string
	who     *Session
	tokens  map[string]rp.Token

	cmu     sync.Mutex
	clients map[string]adminv1connect.AdminServiceClient
}

// LoginAgain is what LoginURL returns: the native console has no page to go
// to, it logs in again.
const LoginAgain = "native:login"

// NewDirect is a Backend over cfg. open shows a URL to the person -- the
// system browser (rp.OpenBrowser).
func NewDirect(cfg DirectConfig, open func(string) error, hc *http.Client) (*Direct, error) {
	if cfg.Issuer == "" || cfg.ClientID == "" {
		return nil, errors.New("console: an issuer and its client_id are needed to log in")
	}
	if len(cfg.Servers) == 0 {
		return nil, errors.New("console: no server to manage")
	}
	for _, s := range cfg.Servers {
		if s.Resource == "" && s.Scope == "" {
			return nil, fmt.Errorf("console: server %q: neither resource nor scope: its token would not be addressed to it alone", s.ID)
		}
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "offline_access"}
	}
	return &Direct{cfg: cfg, open: open, hc: hc, now: time.Now, tokens: map[string]rp.Token{},
		clients: map[string]adminv1connect.AdminServiceClient{}}, nil
}

// Session logs in, the first time, and says who and which servers.
func (d *Direct) Session(ctx context.Context) (*Session, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.who != nil {
		return d.who, nil
	}
	p, err := rp.Discover(ctx, d.hc, d.cfg.Issuer)
	if err != nil {
		return nil, err
	}
	c, tok, id, err := rp.Loopback(ctx, p, rp.Config{ClientID: d.cfg.ClientID, Scopes: d.cfg.Scopes}, d.open, nil)
	if err != nil {
		return nil, err
	}
	if tok.RefreshToken == "" {
		return nil, errors.New("console: the provider issued no refresh token; allow refresh tokens (offline_access) for this client")
	}
	s := &Session{Issuer: d.cfg.Issuer, Subject: id.Subject(), Name: id.Username()}
	for _, sv := range d.cfg.Servers {
		s.Servers = append(s.Servers, Server{ID: sv.ID, Label: sv.Label, Reachable: sv.Issuer == d.cfg.Issuer})
	}
	d.client, d.refresh, d.who = c, tok.RefreshToken, s
	return s, nil
}

// LoginURL is LoginAgain: there is no page to send a native app to.
func (d *Direct) LoginURL(string) string { return LoginAgain }

// Logout forgets the login, and returns where to end the session at the
// provider, if it says.
func (d *Direct) Logout(context.Context) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	end := ""
	if d.client != nil {
		end = d.client.EndSessionURL("")
	}
	d.client, d.refresh, d.who = nil, "", nil
	d.tokens = map[string]rp.Token{}
	return end, nil
}

// token is a current access token for server, refreshed when it is missing,
// expiring, or drop is set (the server refused the last one).
func (d *Direct) token(ctx context.Context, sv DirectServer, drop bool) (string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.client == nil {
		return "", ErrNotLoggedIn
	}
	if t, ok := d.tokens[sv.ID]; ok && !drop && (t.Expiry.IsZero() || d.now().Add(30*time.Second).Before(t.Expiry)) {
		return t.AccessToken, nil
	}
	t, err := d.client.Refresh(ctx, d.refresh, rp.For{Resource: sv.Resource, Scope: sv.Scope})
	if err != nil {
		if rp.IsInvalidGrant(err) {
			d.client, d.refresh, d.who = nil, "", nil
			d.tokens = map[string]rp.Token{}
			return "", ErrNotLoggedIn
		}
		return "", err
	}
	if t.RefreshToken != "" {
		d.refresh = t.RefreshToken
	}
	d.tokens[sv.ID] = *t
	return t.AccessToken, nil
}

// Client is the admin API of server, called directly with its own token.
func (d *Direct) Client(server string) adminv1connect.AdminServiceClient {
	d.cmu.Lock()
	defer d.cmu.Unlock()
	if c, ok := d.clients[server]; ok {
		return c
	}
	i := slices.IndexFunc(d.cfg.Servers, func(s DirectServer) bool { return s.ID == server })
	var c adminv1connect.AdminServiceClient
	if i < 0 {
		c = adminv1connect.NewAdminServiceClient(connect.NewClient(connecthttp.NewTransport(failing{fmt.Errorf("no server %q", server)}, "http://invalid")))
	} else {
		sv := d.cfg.Servers[i]
		hc, err := serverClient(sv)
		var h connecthttp.HTTPClient = &directHTTP{d: d, sv: sv, hc: hc}
		if err != nil {
			h = failing{err}
		}
		c = adminv1connect.NewAdminServiceClient(connect.NewClient(connecthttp.NewTransport(h, sv.URL)))
	}
	d.clients[server] = c
	return c
}

// serverClient is the HTTP client of one server: its CA when it has one.
func serverClient(sv DirectServer) (*http.Client, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12}
	if sv.CAFile != "" {
		pem, err := os.ReadFile(sv.CAFile)
		if err != nil {
			return nil, fmt.Errorf("server %q: %w", sv.ID, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("server %q: %s holds no certificate", sv.ID, sv.CAFile)
		}
		tc.RootCAs = pool
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: &http.Transport{TLSClientConfig: tc, ForceAttemptHTTP2: true,
		Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second}}, nil
}

// directHTTP adds the server's token, and when the server refuses it (401),
// refreshes once and sends the call again.
type directHTTP struct {
	d  *Direct
	sv DirectServer
	hc *http.Client
}

func (h *directHTTP) Do(req *http.Request) (*http.Response, error) {
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
	send := func(drop bool) (*http.Response, error) {
		tok, err := h.d.token(req.Context(), h.sv, drop)
		if err != nil {
			if errors.Is(err, ErrNotLoggedIn) {
				return nil, connect.NewError(connect.CodeUnauthenticated, ErrNotLoggedIn.Error())
			}
			return nil, connect.NewError(connect.CodeUnavailable, err.Error())
		}
		r := req.Clone(req.Context())
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		r.Header.Set("Authorization", "Bearer "+tok)
		return h.hc.Do(r)
	}
	res, err := send(false)
	if err != nil || res.StatusCode != http.StatusUnauthorized {
		return res, err
	}
	res.Body.Close()
	return send(true)
}

// failing answers every call with err: a server that cannot be reached as
// configured.
type failing struct{ err error }

func (f failing) Do(*http.Request) (*http.Response, error) {
	return nil, connect.NewError(connect.CodeFailedPrecondition, f.err.Error())
}
