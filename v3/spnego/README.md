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

> **`KeytabLookup` is called on every authenticated request.** It sits in the request hot path, so a lookup that contacts an external system will add its latency — and its failure modes — to each authentication. If you fetch from a database, a secrets manager, or a remote service, cache the result and refresh it out of band (on a TTL or a rotation event), and apply a bounded timeout.
>
> `NewKeytabFileLookupFunc` already does this: it caches the merged keytab and re-reads the files only when one of them changes size, modification time or identity, so rotating a keytab on disk still takes effect without paying for a parse per request.
>
> Change detection compares each file's size, modification time and identity. On Unix that means replacing a keytab by rename is picked up even when the staging tool preserved the timestamp of a same-sized file, because identity there is the inode. On Windows it is the creation time, which is a hint rather than a dependable identity — NTFS file tunneling can restore a replaced file's creation time, and some filesystems report none — and on other platforms there is no identity at all; both fall back to size and modification time.
>
> Detection is not exact even on Unix: an in-place rewrite that keeps the same size and lands within one filesystem timestamp tick looks unchanged, since the file's identity does not change either. Rotating by rename avoids that case on Unix. Elsewhere, where identity cannot be relied on, make sure the replacement differs in size or modification time.
>
> A keytab that reads but does not parse is treated as a rotation caught mid-write: the last keytab that parsed cleanly keeps being served, and a retry runs at most once a second while the fault lasts. That cover expires after 30 seconds — long enough to absorb a half-written file, short enough that a rotation to a permanently corrupt keytab surfaces as an error instead of silently keeping superseded keys alive. Entering the degraded state logs a warning, expiry logs an error, and recovery logs an all-clear — each once per episode, not per request. A keytab that cannot be read at all — deleted, unmounted, replaced by something unreadable — is reported as an error instead of being masked by the cache.

Note that revoking a keytab with `chmod` alone is not enough: changing permissions moves only the file's ctime, which the cache does not track, so an already-loaded keytab keeps being served until something else changes. Remove or replace the file to revoke it.

### Errors

A failed keytab lookup is answered with a bare `500`. The detail from `NewKeytabFileLookupFunc` names the keytab's path and the underlying OS error — a custom `KeytabLookupFunc` may report whatever it likes — and none of that should reach an unauthenticated caller, so the response body carries only `Internal Server Error`.

The detail is logged at error level instead, throttled to one line per 30 seconds, so a persistent fault cannot be turned into a log flood by unauthenticated callers.

The returned error still matches the package's sentinels, so an application `ErrorHandler` can tell a keytab failure from any other 500:

```go
app := fiber.New(fiber.Config{
	ErrorHandler: func(c fiber.Ctx, err error) error {
		if errors.Is(err, spnego.ErrLookupKeytabFailed) {
			alertOnCall()
		}
		return fiber.DefaultErrorHandler(c, err)
	},
})
```

The status is always `500`: a `*fiber.Error` returned by your own `KeytabLookupFunc` deliberately does not set the response status, since an infrastructure fault reported as, say, `401` would make clients retry credentials against a server that cannot check them.

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

## API Reference

### `New(cfg Config) (fiber.Handler, error)`

Creates a new SPNEGO authentication middleware.

### `GetAuthenticatedIdentityFromContext(ctx FiberContext) (goidentity.Identity, bool)`

Retrieves the authenticated identity from the Fiber context. `FiberContext` is any type exposing `Locals(key any, value ...any) any`, which `fiber.Ctx` satisfies, so handlers call this as `GetAuthenticatedIdentityFromContext(c)`.

### `NewKeytabFileLookupFunc(keytabFiles ...string) (KeytabLookupFunc, error)`

Creates a new KeytabLookupFunc that loads keytab files, caching the merged result until a file changes on disk.

### `NewSystemKeytabLookupFunc() (KeytabLookupFunc, error)`

Creates a KeytabLookupFunc for the host's system keytab: `$KRB5_KTNAME` when set, otherwise `/etc/krb5.keytab`.

MIT Kerberos writes `KRB5_KTNAME` as an optional `TYPE:residual` pair. The file-backed types (`FILE:`, `WRFILE:`) are accepted and stripped; anything else — `KEYRING:`, `MEMORY:`, `ANY:` — is rejected at construction with `ErrUnsupportedKeytabResidualType` rather than being treated as a literal path that fails on every request.

Note that this resolves a *keytab*, not a credential cache. Accepting a SPNEGO ticket requires the service's long-term keys, so a client credential cache (`KRB5CCNAME`) cannot be used to authenticate incoming requests.

## Configuration

The `Config` struct supports the following fields:

- `Next`: A `func(fiber.Ctx) bool` that, when it returns true, skips authentication for that request — useful for health probes and CORS preflights (optional)
- `KeytabLookup`: A function that retrieves the keytab (required, unless `FallbackToSystemKeytab` is set)
- `Log`: A `*log.Logger` receiving gokrb5's diagnostics (optional, defaults to no logging)
- `UseFiberLogger`: Send gokrb5's diagnostics to Fiber's default logger when `Log` is nil (optional, defaults to `false`)
- `Unauthorized`: A `func(fiber.Ctx) error` invoked when a presented Kerberos ticket is rejected, in place of SPNEGO's own rejection response (optional)
- `FallbackToSystemKeytab`: Load the system keytab when `KeytabLookup` is nil, instead of rejecting the configuration (optional, defaults to `false`). The resolved keytab is loaded once during `New`, so a misconfigured host fails at startup rather than on every request.

### Logging

gokrb5 logs once per unauthenticated request, with no level of its own. Since the first leg of every Negotiate handshake is unauthenticated, leaving both `Log` and `UseFiberLogger` unset keeps an unauthenticated client from driving log volume. Setting either one opts in, and note that neither honours Fiber's log level.

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
