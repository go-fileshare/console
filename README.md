# console

The admin console for [go-fileshare](https://github.com/go-fileshare/fileshare).
One page manages the shares, grants and state of **every fileshare server** a
[portal](https://github.com/go-fileshare/portal) knows. It is written once in Go
with [go-widgets](https://github.com/go-widgets) and runs **in a browser
(wasm)** or **as a native window**. Pure Go, `CGO_ENABLED=0`.

This is phase 3 of the [UI design](https://github.com/go-fileshare/fileshare/blob/main/docs/ui.md).

## In a browser, behind the portal

```sh
./build.sh                                  # dist/: console.wasm(.gz), wasm_exec.js, the page
sudo cp -r dist /usr/share/fileshare-console
```

```hcl
# portal.hcl
ui "console" { dir = "/usr/share/fileshare-console" }
```

Open `https://<portal>/console/`. Without a session the console sends you to log
in at your identity provider and comes back. It never holds a token: it calls
`/api/<server>/fileshare.admin.v1.AdminService/<Method>` on the portal, which
adds the token of that server.

What the console does with the portal, through its client package
([`portal/client`](https://github.com/go-fileshare/portal#the-uis-side-client),
which the explorer uses too):

- **A token per server, refreshed once.** When the portal answers that a server's
  token is missing or expiring (`401`, `Portal-Refresh`), the console asks it to
  refresh and sends the call again. It asks for the servers it used most
  recently, at most five, so that switching between two servers does not
  refresh at every click.
- **One refresh at a time, across tabs.** A provider that rotates refresh tokens
  ends the whole login when one is spent twice. Refreshes therefore go through
  the browser's Web Locks API, so two tabs never refresh together, nor a
  console and an explorer tab, which share the session: both take
  `client.RefreshLock`. Before each refresh, the console checks whether
  another tab has just done it.
- **The portal's CSRF defence.** Every call carries `Portal-Request: 1`. The page
  loads no inline script, which the portal's Content-Security-Policy would
  refuse.

The page weighs **5.5 MB compressed** (23 MB of wasm; `build.sh` writes the
`.gz` and the portal sends it). It used to be 7.1 MB. `internal/adminpb` is
generated here from fileshare's `admin.proto`, the messages and a Connect client
only, because fileshare's own package carries gRPC stubs, and those alone put
3 MB of compressed gRPC in the page. CI checks that the `.proto` is byte for byte
fileshare v0.29.0's, and that the generated code is what the pinned generators
make of it.

## Natively

```sh
go install github.com/go-fileshare/console@latest
console                     # reads ~/Library/Application Support/fileshare-console/console.hcl (macOS)
console -config my.hcl      # or $XDG_CONFIG_HOME/fileshare-console/console.hcl elsewhere
console -demo               # over a built-in stand-in, to see it without servers
```

Natively there is no portal: the console logs in itself and calls each server
directly.

```hcl
issuer "https://login.example.org" {
  client_id = "fileshare-console"     # a PUBLIC client: no secret
}

server "paris" {
  label    = "Paris"
  url      = "https://fs-paris.example.org:8443"   # fileshare's admin { web { listen } }
  resource = "https://fs-paris.example.org/"       # that server's audience
  # ca_file = "fs-ca.pem"                          # relative to this file
}
```

- **Logging in** goes through the **system browser** and a listener on
  `127.0.0.1`, with [go-authn/rp](https://github.com/go-authn/rp) (RFC 8252):
  PKCE, state, nonce, and a knock on the port with another `state` is ignored.
  The client is public, because a secret shipped inside an application is no
  secret. [go-authn/bridge](https://github.com/go-authn/bridge) accepts any
  loopback port for a client registered with `http://127.0.0.1/callback`.
- **Each server gets a token addressed to it alone** (`resource`, RFC 8707),
  bought with the refresh token. The refresh token stays **in memory only** and
  is spent one refresh at a time. Without that lock, two concurrent refreshes
  trigger the provider's reuse detection and the login is lost; the test checks
  exactly that.
- A server that refuses its token gets one refresh and one retry. A grant that
  has ended means logging in again, in the browser.
- **Only https servers.** A bearer token sent in the clear is anybody's.
- **No redirect is followed** (since v0.3.2). Go's client keeps
  `Authorization` across a redirect to the same host on any scheme or port, so
  a `307 http://...` from a misconfigured proxy would have sent the token in
  clear. go-authn/rp v0.1.1 does the same for the provider's token endpoint.

## What it does

| | |
|---|---|
| **servers** | every server the portal knows; one behind another identity provider is listed but not reachable from this login |
| **shares** | name, source, enabled or not; details: kind of source, configuration file or admin API, filesystem, read-only and why, size, protocols, what it is served over, and why not over the others |
| **create** | a directory share, with its first grant (fileshare refuses a share nobody may use) |
| **enable / disable** | disabling closes its connections and lets go of its source |
| **delete** | two presses; the files stay where they are |
| **grant / revoke** | `alice`, `@group`, `oidc:groups:<group>`, `oidc:user:<name>`, as in fileshare's configuration file; revoking the last grant is refused by the server |
| **volumes** | on a server with fileshare's provisioner: the parents (ZFS, btrfs, XFS, ext4) with their free space; each volume's quota, use, snapshots and the shares that use it. **Create** (`500G`, `2T`: binary units, at most the server's `max_volume`), **resize** (never below what is used), **snapshot** (ZFS and btrfs only; elsewhere the button is greyed), **share it** (the share form gets `volume:<parent>/<name>` as its source), **delete** (two presses, refused while a share uses it). The console **never destroys data**: a volume that holds data or snapshots is refused by the server, and the console says so. Destroying data is not something to do from a list with a button. A server without a provisioner is shown as such, not as a failure |

## How it is built

- **MVVM, enforced.** `viewmodel.go` holds all state as
  [go-widgets/mvvm](https://github.com/go-widgets/mvvm) observables and imports no
  widget. `scene.go` binds widgets to it through
  [mvvmtk](https://github.com/go-widgets/mvvmtk) and never writes widget state.
  CI runs [mvvmlint](https://github.com/go-widgets/mvvmlint) and
  [bricolint](https://github.com/go-widgets/bricolint), each with a control
  showing that it fails on a violation.
- **Network results reach the screen safely.** Every call runs on its own
  goroutine and posts its result to an `mvvm.Queue`. The root container drains
  the queue in `SetBounds`, which every
  [window](https://github.com/go-widgets/window) back-end calls before drawing.
  The queue's wake is the back-end's `Repaint`, so a result shows up with nobody
  touching the window, in a browser tab as in a native one. A result about a
  server no longer selected is dropped.
- **One tree, two targets.** `window.Open` picks the back-end; `w.Run(root)` is the
  same call for both.

## Tests

```sh
go test -race ./...
```

- **ViewModel tests:** the real portal client against a stand-in for the portal
  and two fileshare servers (`internal/admin/admintest`, real Connect handlers).
  They cover login, listing, every command, failures, a login that ends, and a
  late answer about another server.
- **Portal client:** one refresh for ten calls at once; the servers in use kept
  together; falling back to one server past the portal's cookie budget; the
  static header on every call.
- **View:** built, bound, laid out and drawn natively. A click reaches the
  command, and a result posted from a goroutine is applied by the next frame and
  not before.
- **`e2e/`:** the console as deployed. `console.wasm` runs in headless Chrome,
  behind the portal binary over TLS, in front of two fileshare binaries, with a
  test OpenID provider. The browser is sent to log in, comes back, lists A,
  creates a share there, and A's audit names `oidc=<issuer> alice-sub` while B
  audits nothing. With go-widgets/window v0.87.0, whose browser tab could not
  be woken, the same test stays on "Loading...": the answers wait in the queue
  for a frame that never comes. A second test runs the native backend against
  the same two real servers: loopback login as a public client, a token per
  server, a share created on A and audited there.
- **Volumes:** a stand-in server with a provisioner (ZFS that snapshots, XFS
  that does not). Covered: create (and the same again), resize (refused below
  use before the server is asked), snapshot, share it, delete refused while
  shared, refused when it holds data, deleted when empty, a server without a
  provisioner, and every form refusal before the server is asked. Each of these
  makes a test fail when removed: sending `destroy_data`, deleting in one
  press, a resize below use reaching the server, no reload after a share is
  deleted, a missing provisioner shown as a failure, snapshots offered
  everywhere. Against real fileshare, the browser test checks the
  no-provisioner page.
- **Native backend:** in-process provider (public client, loopback, rotating
  refresh tokens) and servers that verify their audience as fileshare does.
  Covered: one login per session, one refresh at a time for 20 concurrent calls,
  a retry after a 401, an ended grant, logout, and a server reached only
  through its `ca_file`. Removing the lock, the resource, the drop of a refused
  token or the `invalid_grant` handling makes a test fail.

Each defence was removed in turn, and a test failed each time:

- the refresh lock;
- the "already refreshed" check;
- keeping the servers in use;
- the static header;
- the late-answer check;
- the two-press delete;
- the redirect to login;
- draining the queue at the frame.

## Licence

BSD 3-Clause.
