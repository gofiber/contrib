package spnego

import "errors"

// ErrConfigInvalidOfKeytabLookupFunctionRequired is returned when the KeytabLookup function is not set in Config
var ErrConfigInvalidOfKeytabLookupFunctionRequired = errors.New("config invalid: keytab lookup function is required")

// ErrLookupKeytabFailed is returned when the keytab lookup fails
var ErrLookupKeytabFailed = errors.New("keytab lookup failed")

// ErrConfigInvalidOfAtLeastOneKeytabFileRequired is returned when no keytab
// files are provided, and when one of the paths is empty — the shape an unset
// environment variable takes. Neither names a keytab file.
var ErrConfigInvalidOfAtLeastOneKeytabFileRequired = errors.New("config invalid: at least one keytab file required")

// ErrLoadKeytabFileFailed is returned when load keytab files failed
var ErrLoadKeytabFileFailed = errors.New("load keytab failed")

// ErrUnsupportedKeytabResidualType is returned when KRB5_KTNAME names a keytab
// type that is not backed by a plain file, such as KEYRING or MEMORY.
var ErrUnsupportedKeytabResidualType = errors.New("unsupported keytab residual type")

// ErrConfigInvalidOfNegativeMaxClockSkew is returned when Config.MaxClockSkew
// is negative, which is otherwise indistinguishable from leaving it unset.
var ErrConfigInvalidOfNegativeMaxClockSkew = errors.New("config invalid: max clock skew must not be negative")

// ErrConfigInvalidOfKeytabPrincipalRealm is returned when Config.KeytabPrincipal
// carries an "@REALM" suffix, which gokrb5 drops without reporting — so
// accepting it would silently pin a different principal than the one written.
var ErrConfigInvalidOfKeytabPrincipalRealm = errors.New("config invalid: keytab principal must not include a realm")

// errNilKeytab is returned when a KeytabLookupFunc reports success but yields
// no keytab. It is internal: callers see ErrLookupKeytabFailed.
var errNilKeytab = errors.New("keytab lookup returned no keytab")

// ErrSPNEGOHandlerFailed is returned, and passed to Config.OnError, when gokrb5
// fails inside its own handler rather than reaching an authentication outcome: a
// SessionManager whose New could not persist, or a panic out of gokrb5 itself.
// A failed Get does not land here — gokrb5 falls through to full validation.
//
// It is told from an authentication outcome by the error the session manager
// returned, and failing that by WWW-Authenticate — never by the status, which
// is whatever a manager holding the raw ResponseWriter wrote first.
var ErrSPNEGOHandlerFailed = errors.New("spnego handler reported an internal failure")
