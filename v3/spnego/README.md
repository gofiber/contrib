---
id: spnego
---

# SPNEGO Kerberos Authentication Middleware for Fiber

![Release](https://img.shields.io/github/v/tag/gofiber/contrib?filter=*spnego*)
[![Discord](https://img.shields.io/discord/704680098577514527?style=flat&label=%F0%9F%92%AC%20discord&color=00ACD7)](https://gofiber.io/discord)
![Test](https://github.com/gofiber/contrib/workflows/Test%20spnego/badge.svg)

This middleware provides SPNEGO (Simple and Protected GSSAPI Negotiation Mechanism) authentication for [Fiber](https://github.com/gofiber/fiber) applications, enabling Kerberos authentication for HTTP requests and inspired by [gokrb5](https://github.com/jcmturner/gokrb5)

## Features

- Kerberos authentication via SPNEGO mechanism
- Flexible keytab lookup system
- Support for dynamic keytab retrieval from various sources
- Integration with Fiber context for authenticated identity storage
- Group-based authorization from Active Directory's PAC, with no extra parsing
- Optional sessions, so an authenticated client is not revalidated per request
- Keytab principal, clock skew and host-address controls
- Success and failure hooks for metrics and audit
- Configurable logging

## Version Compatibility

This middleware is compatible with:

- **Fiber v3**

## Installation

```bash
# For Fiber v3
go get github.com/gofiber/contrib/v3/spnego
```

## Usage

```go
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gofiber/contrib/v3/spnego"
	"github.com/gofiber/contrib/v3/spnego/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
)

func main() {
	app := fiber.New()
	// Create a configuration with a keytab lookup function
	// For testing, you can create a mock keytab file using utils.NewMockKeytab
	// In production, use a real keytab file
	// A temporary directory keeps key material out of the working directory.
	tempDir, err := os.MkdirTemp("", "spnego-example")
	if err != nil {
		panic(fmt.Errorf("create temp dir failed: %w", err))
	}
	// panic rather than log.Fatalf below: Fatalf calls os.Exit, which would
	// skip these deferred cleanups and leave the keytab on disk.
	defer func() { _ = os.RemoveAll(tempDir) }()
	keytabPath := filepath.Join(tempDir, "temp-sso.keytab")

	_, clean, err := utils.NewMockKeytab(
		utils.WithPrincipal("HTTP/sso1.example.com"),
		utils.WithRealm("EXAMPLE.LOCAL"),
		utils.WithFilename(keytabPath),
		utils.WithPairs(utils.EncryptTypePair{
			Version:     2,
			EncryptType: 18,
			CreateTime:  time.Now(),
		}),
	)
	if err != nil {
		panic(fmt.Errorf("create mock keytab failed: %w", err))
	}
	defer clean()
	keytabLookup, err := spnego.NewKeytabFileLookupFunc(keytabPath)
	if err != nil {
		panic(fmt.Errorf("create keytab lookup failed: %w", err))
	}
	
	// Create the middleware
	authMiddleware, err := spnego.New(spnego.Config{
		KeytabLookup: keytabLookup,
	})
	if err != nil {
		panic(fmt.Errorf("create middleware failed: %w", err))
	}

	// Apply the middleware to protected routes
	app.Use("/protected", authMiddleware)

	// Access authenticated identity
	app.Get("/protected/resource", func(c fiber.Ctx) error {
		identity, ok := spnego.GetAuthenticatedIdentityFromContext(c)
		if !ok {
			return c.Status(fiber.StatusUnauthorized).SendString("Unauthorized")
		}
		return c.SendString(fmt.Sprintf("Hello, %s!", identity.UserName()))
	})

	log.Info("Starting server on :3000")
	if err := app.Listen(":3000"); err != nil {
		panic(fmt.Errorf("start server failed: %w", err))
	}
}
```

## Dynamic Keytab Lookup

The middleware is designed with extensibility in mind, allowing keytab retrieval from various sources beyond static files:

> **`KeytabLookup` is called on every request the middleware handles, authenticated or not.** The lookup runs before the ticket is examined, so a client that presents no credentials at all still drives one call per request — size any rate limit or timeout for traffic you do not control. It also sits in the request hot path, so a lookup that contacts an external system adds its latency, and its failure modes, to every request. If you fetch from a database, a secrets manager, or a remote service, cache the result and refresh it out of band (on a TTL or a rotation event), and apply a bounded timeout.
>
> `NewKeytabFileLookupFunc` already does this: it caches the merged keytab and re-reads the files only when one of them changes size, modification time or identity, so rotating a keytab on disk still takes effect without paying for a parse per request. What it does still pay is one `stat` per keytab file per request, which is what makes a rotation — or a revocation — take effect on the next request rather than after some interval. If that cost matters more to you than immediacy, supply your own `KeytabLookup` that re-reads on a timer.
>
> Change detection compares each file's size, modification time and identity. On Unix that means replacing a keytab by rename is picked up even when the staging tool preserved the timestamp of a same-sized file, because identity there is the inode. On Windows it is the creation time, which is a hint rather than a dependable identity — NTFS file tunneling can restore a replaced file's creation time, and some filesystems report none — and on other platforms there is no identity at all; both fall back to size and modification time.
>
> Detection is not exact even on Unix: an in-place rewrite that keeps the same size and lands within one filesystem timestamp tick looks unchanged, since the file's identity does not change either. Rotating by rename avoids that case on Unix. Elsewhere, where identity cannot be relied on, make sure the replacement differs in size or modification time.
>
> A keytab that reads but does not parse is treated as a rotation caught mid-write: the last keytab that parsed cleanly keeps being served, and a retry runs at most once a second while the fault lasts. That cover expires after 30 seconds — long enough to absorb a half-written file, short enough that a rotation to a permanently corrupt keytab surfaces as an error instead of silently keeping superseded keys alive. Entering the degraded state logs a warning, expiry logs an error, and recovery logs an all-clear at warning level too, so a deployment filtering below warnings still sees each alert close. Each is emitted once per episode rather than per request, and an episode can only begin when the files on disk change, so the rate follows rotations rather than request volume. Note that the lines are not ordered against each other across concurrent requests: an all-clear can occasionally print above the warning it clears, so match them on content rather than on arrival order. The lines are written after the reload lock is released, so a blocking log sink cannot queue every other authenticated request behind it — though the request that triggered the transition does wait for the write, as it would for any log call. A keytab that cannot be read at all — deleted, unmounted, replaced by something unreadable — is reported as an error instead of being masked by the cache.

Note that revoking a keytab with `chmod` alone is not enough: changing permissions moves only the file's ctime, which the cache does not track, so an already-loaded keytab keeps being served until something else changes. Remove or replace the file to revoke it.

A `KeytabLookupFunc` is any function returning a `*keytab.Keytab`, so the source is yours to choose:

```go
// Example: Retrieve keytab from a database
func dbKeytabLookup() (*keytab.Keytab, error) {
    // Your database lookup logic here
    // ...
    return keytabFromDatabase, nil
}

// Example: Retrieve keytab from a remote service
func remoteKeytabLookup() (*keytab.Keytab, error) {
    // Your remote service call logic here
    // ...
    return keytabFromRemote, nil
}
```

### Errors

A failed keytab lookup is answered with a bare `500`. The detail from `NewKeytabFileLookupFunc` names the keytab's path and the underlying OS error — a custom `KeytabLookupFunc` may report whatever it likes — and none of that should reach an unauthenticated caller, so the response body carries only `Internal Server Error`.

The detail is logged at error level instead, throttled to one line per 30 seconds, so a persistent fault cannot be turned into a log flood by unauthenticated callers.

The status is always `500`: a `*fiber.Error` returned by your own `KeytabLookupFunc` deliberately does not set the response status, since an infrastructure fault reported as, say, `401` would make clients retry credentials against a server that cannot check them.

There are two sentinels, and both reach the `ErrorHandler` and `OnError` the same way, with the same sanitised body:

| Sentinel | Raised when |
| --- | --- |
| `ErrLookupKeytabFailed` | `KeytabLookup` returned an error, or no keytab |
| `ErrSPNEGOHandlerFailed` | gokrb5 stopped short of an authentication outcome — in v8.4.4, a `SessionManager` that could not persist a session |

The returned error still matches them, so an application `ErrorHandler` can tell either from any other 500. Match on both: matching only the first misses every broken-session-store failure.

```go
app := fiber.New(fiber.Config{
	ErrorHandler: func(c fiber.Ctx, err error) error {
		if errors.Is(err, spnego.ErrLookupKeytabFailed) ||
			errors.Is(err, spnego.ErrSPNEGOHandlerFailed) {
			alertOnCall()
		}
		return fiber.DefaultErrorHandler(c, err)
	},
})
```

## API Reference

### `New(cfg Config) (fiber.Handler, error)`

Creates a new SPNEGO authentication middleware.

### `GetAuthenticatedIdentityFromContext(ctx FiberContext) (goidentity.Identity, bool)`

Retrieves the authenticated identity from the Fiber context. `FiberContext` is any type exposing `Locals(key any, value ...any) any`, which `fiber.Ctx` satisfies, so handlers call this as `GetAuthenticatedIdentityFromContext(c)`.

### `NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error)`

Creates a new KeytabLookupFunc that loads keytab files, caching the merged result until a file changes on disk.

### `NewSystemKeytabLookupFunc() (KeytabLookupFunc, error)`

Creates a KeytabLookupFunc for the host's system keytab: `$KRB5_KTNAME` when set, otherwise `/etc/krb5.keytab`.

MIT Kerberos writes `KRB5_KTNAME` as an optional `TYPE:residual` pair. The file-backed types (`FILE:`, `WRFILE:`) are accepted and stripped, and the known types that cannot be read as a file (`KEYRING:`, `MEMORY:`, `DIR:`, `ANY:`, `SRVTAB:`) are rejected at construction with `ErrUnsupportedKeytabResidualType` rather than being treated as a literal path that fails on every request.

A prefix matching no known type is taken as part of the filename, since a relative path may legitimately contain a colon. A typo such as `FIEL:/etc/krb5.keytab` is therefore resolved as a path and fails when the file cannot be found — at construction if `FallbackToSystemKeytab` is set, otherwise on the first request.

Note that this resolves a *keytab*, not a credential cache. Accepting a SPNEGO ticket requires the service's long-term keys, so a client credential cache (`KRB5CCNAME`) cannot be used to authenticate incoming requests.

## Configuration

The `Config` struct supports the following fields:

- `Next`: A `func(fiber.Ctx) bool` that, when it returns true, skips authentication for that request — useful for health probes and CORS preflights (optional)
- `KeytabLookup`: A function that retrieves the keytab (required, unless `FallbackToSystemKeytab` is set)
- `Log`: A `*log.Logger` receiving gokrb5's diagnostics (optional, defaults to no logging)
- `UseFiberLogger`: Send gokrb5's diagnostics to Fiber's default logger when `Log` is nil (optional, defaults to `false`)
- `Unauthorized`: A `func(fiber.Ctx) error` invoked when a presented Kerberos ticket is rejected, in place of SPNEGO's own rejection response (optional)
- `FallbackToSystemKeytab`: Load the system keytab when `KeytabLookup` is nil, instead of rejecting the configuration (optional, defaults to `false`). The resolved keytab is loaded once during `New`, so a misconfigured host fails at startup rather than on every request.
- `KeytabPrincipal`: Pin which principal's key decrypts an incoming ticket, out of a keytab holding several (optional, defaults to `""` — gokrb5 infers it from the ticket). See [Restricting the accepted principal](#restricting-the-accepted-principal).
- `MaxClockSkew`: How far a ticket's issue time may sit from this host's clock (optional, defaults to `0`, which leaves gokrb5's own 5 minutes). A negative value is rejected by `New`.
- `DisablePACDecoding`: Turn off decoding of Active Directory's PAC (optional, defaults to `false` — the PAC *is* decoded). Decoding is what populates the group SIDs behind `Identity.Authorized`, so only disable it on a service that never inspects groups.
- `RequireHostAddress`: Reject a ticket that carries no host addresses (optional, defaults to `false`)
- `SessionManager`: A `service.SessionMgr` letting gokrb5 serve later requests from a session instead of revalidating a ticket each time (optional, defaults to `nil`). See [Sessions](#sessions).
- `OnSuccess`: A `func(fiber.Ctx, goidentity.Identity)` called once per authenticated request, before the rest of the chain (optional, defaults to `nil`)
- `OnError`: A `func(fiber.Ctx, error)` called when the middleware itself fails — a keytab lookup failure, or a failure inside the SPNEGO handler such as a session store that cannot be reached — just before the response (optional, defaults to `nil`)

`ConfigDefault` records these defaults if you would rather start from it than from a zero `Config` — the two are equal. `New` applies them by treating a zero field as unset rather than by reading the variable, so copy it; mutating it changes nothing.

### Group-based authorization

Against Active Directory the middleware gives you more than a username. AD embeds a PAC in the ticket, gokrb5 decodes it by default, and the caller's group SIDs land on the identity — so authorization needs no extra parsing:

```go
const admins = "S-1-5-21-1004336348-1177238915-682003330-512"

app.Get("/admin", authMiddleware, func(c fiber.Ctx) error {
    identity, ok := spnego.GetAuthenticatedIdentityFromContext(c)
    if !ok || !identity.Authorized(admins) {
        return fiber.ErrForbidden
    }
    return c.SendString("Hello, " + identity.UserName())
})
```

`identity.AuthzAttributes()` returns every SID if you would rather match against a set. For the full AD payload — effective name, logon domain, primary group — type-assert to gokrb5's concrete type:

```go
if creds, ok := identity.(*credentials.Credentials); ok {
    ad := creds.GetADCredentials()
    log.Printf("%s logged on from %s", ad.EffectiveName, ad.LogonDomainName)
}
```

MIT Kerberos does not normally issue a PAC, so on a non-AD realm expect `AuthzAttributes()` to be empty and authorize on the principal name instead.

### Restricting the accepted principal

Left unset, gokrb5 infers the decrypting key from the ticket, so a ticket naming **any** principal in the keytab is accepted. That is the right behaviour for one service with one SPN, and the wrong one as soon as a keytab is merged from several: a client entitled to one of them gets into all of them.

`KeytabPrincipal` pins the key, so a ticket minted for a different principal fails to decrypt and is refused. Set it whenever the keytab covers more than the service sitting behind it.

```go
authMiddleware, err := spnego.New(spnego.Config{
    KeytabLookup:    keytabLookup,
    KeytabPrincipal: "HTTP/sso.example.com",
})
```

Give the principal without a realm. gokrb5 parses this with `types.ParseSPNString`, which drops anything after an `@` without reporting it, so `New` rejects a value containing one rather than letting it look effective.

If you went looking for gokrb5's `service.SName` and wondered why it is not exposed: it has no effect here. In v8.4.4 the SPNEGO accept path runs `service.VerifyAPREQ`, which never reads it — the only reader is `KRB5BasicAuthenticator`, a different flow. Wiring it up would promise a restriction that does nothing.

### Sessions

`SessionManager` is the largest saving available on an authenticated hot path: gokrb5 establishes a session after the first successful authentication and serves later requests from it, skipping ticket validation entirely.

It also changes the trust model. The session becomes a credential in its own right, so whatever your implementation stores must be unguessable, bound to the client, and expired deliberately — a predictable session identifier is a full authentication bypass. The middleware forwards the request's cookies to gokrb5 only when this is set.

What the manager writes is replayed selectively. **Headers** — `Set-Cookie` included — reach the Fiber response on any request that gets to an authentication outcome, meaning one that authenticates as well as one that gets a challenge or a refusal. The **body and status do not**, ever: they belong to the handler the request is being authenticated for, and are overwritten by it. Report through headers, or through your own logger. On a request that instead fails inside the handler, all three are dropped, so a cookie written just before the failure never advertises a session that was not stored.

The request handed to your manager is built by this middleware rather than by `net/http`, so it carries only what gokrb5 reads plus what a session store needs to make its decisions: method, host, remote address, cookies, protocol, `URL.Scheme`, `TLS`, and the context. `Body` is `http.NoBody` — non-nil, so the usual `defer r.Body.Close()` is safe, but empty. `RequestURI` and the URL's path and query are deliberately absent, so do not key a session on them.

One thing on it is credential material: `Authorization` carries the client's base64 Kerberos ticket, because that is what gokrb5 reads. Do not log or serialise `r.Header` wholesale from a manager.

Only `Flush` is provided beyond the `http.ResponseWriter` methods themselves, and it buffers rather than sending — the middleware has to see the whole response before it can tell an authentication outcome from a failure, so nothing streams through here. Probe for anything else with comma-ok or `http.ResponseController`; a bare `w.(http.Hijacker)` panics, and there is no connection behind this writer to hijack.

`r.Context()` is Fiber's, so a deadline or a tracing span installed by middleware upstream reaches your store through the usual `QueryContext`. Do not expect more of it than the application put there: Fiber's context is `context.Background()` unless something calls `SetContext`, and Fiber does not cancel it when a client disconnects.

Its strings are yours to keep. Fiber hands out header and URI values as views into fasthttp's request buffer, and fasthttp pools those buffers across connections — so retaining one normally means reading back whichever client came next, not just more of the same one. This middleware copies them before building the request, so a manager that stores `r.Host` alongside a session records the host it actually saw. The copies are made only when `SessionManager` is set, since nothing else outlives the buffer.

The context is the one field that promise cannot cover, because its contents are the application's. `*fasthttp.RequestCtx` satisfies `context.Context`, so an app that does `c.SetContext(context.WithValue(c.RequestCtx(), k, v))` hands your manager a value chain reaching into the very buffer Fiber recycles. If you retain anything you pulled out of the context, copy it.

`Secure: r.TLS != nil` — the usual idiom — is reliable only where Fiber terminates TLS itself. Behind a TLS-terminating proxy the connection into Fiber is plaintext, so `TLS` is nil.

`URL.Scheme` is not a drop-in replacement: Fiber returns `http` regardless of `X-Forwarded-Proto` unless `Config.TrustProxy` is on **and** the peer matches `TrustProxyConfig`. So behind a proxy, both fields can say "not secure" for a connection the client made over HTTPS. Either configure `TrustProxy` for your proxy's address and then trust the scheme, or set `Secure` unconditionally if the service is only ever reached over HTTPS.

The two failure paths are not symmetric, which matters for monitoring. A manager whose `New` cannot persist makes gokrb5 abandon the request from inside its handler; the middleware treats that as an internal failure rather than an ordinary response — logged on its own throttle, passed to `OnError` as `ErrSPNEGOHandlerFailed`, and answered with the same sanitised `Internal Server Error` body a keytab failure gets, so a manager that reports a DSN or a driver error cannot leak it to an unauthenticated caller. A manager whose `Get` fails is different: gokrb5 discards that error and falls through to full ticket validation, so a broken read path degrades performance silently and is worth watching on the store's own side.

The two are told apart by the error your `New` returns, and by `WWW-Authenticate` — never by the status. The status is only ever the first one written, and a manager holding the raw `ResponseWriter` can write its own before it fails, which would otherwise mask the failure as an ordinary `200` or `4xx`.

One caveat for whoever is on call. gokrb5 sends your manager's actual error to *its own* logger and writes only `Internal Server Error` to the response, so that boilerplate is what `ErrSPNEGOHandlerFailed` carries unless your manager wrote something itself. To see why the store refused, set `Log` or `UseFiberLogger` — or log it inside your manager, which is the more direct route.

### Observability

`OnSuccess` and `OnError` are for metrics and audit; neither can change the outcome.

```go
authMiddleware, err := spnego.New(spnego.Config{
    KeytabLookup: keytabLookup,
    OnSuccess: func(c fiber.Ctx, identity goidentity.Identity) {
        authSuccesses.Inc()
    },
    OnError: func(c fiber.Ctx, err error) {
        authInternalErrors.Inc()
    },
})
```

The two do not cover every outcome, deliberately. `OnSuccess` fires only for a request that authenticated — a challenge, a continuation and a rejection are all *not* authenticated, and counting them as successes would overstate logins. `OnError` fires only when the middleware itself fails; a refused ticket is `Unauthorized`'s business, not an internal error. The error `OnError` receives carries the underlying cause, which names keytab paths and OS errors, so treat it as internal diagnostics rather than something to echo back.

### Behind a reverse proxy

gokrb5 binds ticket validation to the connection's peer address, which behind a proxy is the proxy rather than the client. A client presenting an address-restricted ticket will therefore be refused.

The middleware deliberately does **not** read `X-Forwarded-For` here: doing so would let a client choose the address its own ticket is validated against, which defeats the restriction entirely. If your clients use address-restricted tickets, terminate Kerberos at the edge or issue tickets without address restrictions.

### Logging

gokrb5 writes a line for every request carrying a `Negotiate` token, with no level of its own: one on success, one on a refusal, and one on a token it cannot parse. On a busy service that is a line per request, so enabling this is not cheap on authenticated traffic. It is silent only for requests it declines to negotiate at all — no `Authorization` header, or one for a different scheme.

One case escapes that rule: gokrb5 parses the client address before it looks at the token and logs when it cannot, so a listener whose remote addresses are not `host:port` — a Unix socket, for instance — produces a line on every request, including the opening challenge.

Leaving both `Log` and `UseFiberLogger` unset keeps all of that off the log. Setting either one opts in, and note that neither honours Fiber's log level.

### Customizing the unauthorized response

```go
authMiddleware, err := spnego.New(spnego.Config{
	KeytabLookup: keytabLookup,
	Unauthorized: func(c fiber.Ctx) error {
		return c.Status(fiber.StatusUnauthorized).JSON(fiber.Map{
			"error": "kerberos authentication required",
		})
	},
})
```

**This handler runs only when Kerberos authentication is actually refused** — when a client presented a ticket the service rejected.

gokrb5 answers `401` in three different situations, and only one of them is a failure:

| WWW-Authenticate | Meaning | Reaches your handler |
|---|---|---|
| `Negotiate` (bare) | Opening challenge that starts the handshake | No |
| `Negotiate oRQwEqADCgEB…` | Continuation token: retry with KRB5 | No |
| `Negotiate oQcwBaADCgEC` | Ticket rejected | **Yes** |

(Success is signalled by `Negotiate oRQwEqADCgEA…`, which differs from the continuation value only at the twelfth base64 character — `CgEA` versus `CgEB`.)

The first two are steps in a negotiation that can still succeed. `curl --negotiate` and every major browser begin SPNEGO only when an untouched `401` arrives, so rewriting either one — changing the status to `403`, say — would stop authentication from ever completing. They are always passed straight through, and your handler never sees them.

One limitation worth knowing: gokrb5 sends the continuation value not only for a genuine continue, but also for a `Negotiate` header that fails base64 decoding or does not unmarshal as SPNEGO or raw KRB5. An NTLM-only client is therefore indistinguishable from a client mid-handshake, and those failures never reach your handler. Classifying them as rejections instead would break real continuations, which is the worse failure.

The rejection response's headers are already set when your handler runs, so setting a status and body leaves them in place. Removing the `WWW-Authenticate` header will break negotiation.

## Requirements

- Fiber v3
- Kerberos infrastructure

## Notes

- Ensure your Kerberos infrastructure is properly configured
- The middleware handles the SPNEGO negotiation process
- Retrieve authenticated identities with `GetAuthenticatedIdentityFromContext`
