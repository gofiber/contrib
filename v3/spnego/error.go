package spnego

import "errors"

// ErrConfigInvalidOfKeytabLookupFunctionRequired is returned when the KeytabLookup function is not set in Config
var ErrConfigInvalidOfKeytabLookupFunctionRequired = errors.New("config invalid: keytab lookup function is required")

// ErrLookupKeytabFailed is returned when the keytab lookup fails
var ErrLookupKeytabFailed = errors.New("keytab lookup failed")

// ErrConfigInvalidOfAtLeastOneKeytabFileRequired is returned when no keytab files are provided
var ErrConfigInvalidOfAtLeastOneKeytabFileRequired = errors.New("config invalid: at least one keytab file required")

// ErrLoadKeytabFileFailed is returned when load keytab files failed
var ErrLoadKeytabFileFailed = errors.New("load keytab failed")

// ErrUnsupportedKeytabResidualType is returned when KRB5_KTNAME names a keytab
// type that is not backed by a plain file, such as KEYRING or MEMORY.
var ErrUnsupportedKeytabResidualType = errors.New("unsupported keytab residual type")

// ErrConfigInvalidOfNegativeMaxClockSkew is returned when Config.MaxClockSkew
// is negative. Passing it on would be indistinguishable from leaving it unset,
// so the service would run on gokrb5's five minutes while the configuration
// said otherwise.
var ErrConfigInvalidOfNegativeMaxClockSkew = errors.New("config invalid: max clock skew must not be negative")

// ErrConfigInvalidOfKeytabPrincipalRealm is returned when
// Config.KeytabPrincipal carries an "@REALM" suffix. gokrb5 parses the value
// with types.ParseSPNString, which drops the realm without reporting anything,
// so accepting it would silently pin a different principal than the one written.
var ErrConfigInvalidOfKeytabPrincipalRealm = errors.New("config invalid: keytab principal must not include a realm")

// errNilKeytab is returned when a KeytabLookupFunc reports success but yields
// no keytab. It is internal: callers see ErrLookupKeytabFailed.
var errNilKeytab = errors.New("keytab lookup returned no keytab")

// ErrSPNEGOHandlerFailed is returned, and passed to Config.OnError, when gokrb5
// answers 5xx from inside its own handler rather than reaching an
// authentication outcome. In gokrb5 v8.4.4 that means one thing: a
// Config.SessionManager whose New could not persist a session, which
// spnego/http.go reports through spnegoInternalServerError.
//
// A session manager whose Get fails does not land here. gokrb5 discards that
// error and falls through to full ticket validation, so a session store with a
// broken read path degrades performance silently rather than failing requests.
//
// It is exported because it crosses the API boundary: without it a caller
// receiving the error from OnError or an ErrorHandler could only classify it by
// matching on text.
var ErrSPNEGOHandlerFailed = errors.New("spnego handler reported an internal failure")
