package spnego

import "errors"

// ErrConfigInvalidOfKeytabLookupFunctionRequired is returned when the KeytabLookup function is not set in Config
var ErrConfigInvalidOfKeytabLookupFunctionRequired = errors.New("config invalid: keytab lookup function is required")

// ErrLookupKeytabFailed is returned when the keytab lookup fails
var ErrLookupKeytabFailed = errors.New("keytab lookup failed")

// ErrConfigInvalidOfAtLeastOneKeytabFileRequired is returned when no keytab
// files are given, or one path is empty — an unset environment variable.
var ErrConfigInvalidOfAtLeastOneKeytabFileRequired = errors.New("config invalid: at least one keytab file required")

// ErrLoadKeytabFileFailed is returned when load keytab files failed
var ErrLoadKeytabFileFailed = errors.New("load keytab failed")

// ErrUnsupportedKeytabResidualType is returned when KRB5_KTNAME names a keytab
// type that is not backed by a plain file, such as KEYRING or MEMORY.
var ErrUnsupportedKeytabResidualType = errors.New("unsupported keytab residual type")

// ErrConfigInvalidOfNegativeMaxClockSkew is returned when Config.MaxClockSkew
// is negative, which is otherwise indistinguishable from unset.
var ErrConfigInvalidOfNegativeMaxClockSkew = errors.New("config invalid: max clock skew must not be negative")

// ErrConfigInvalidOfKeytabPrincipalRealm is returned for an "@REALM" suffix,
// which gokrb5 drops silently — pinning a different principal than written.
var ErrConfigInvalidOfKeytabPrincipalRealm = errors.New("config invalid: keytab principal must not include a realm")

// errNilKeytab is returned when a KeytabLookupFunc reports success but yields
// no keytab. It is internal: callers see ErrLookupKeytabFailed.
var errNilKeytab = errors.New("keytab lookup returned no keytab")

// ErrSPNEGOHandlerFailed is returned, and passed to Config.OnError, when gokrb5
// fails inside its handler: a SessionManager New that could not persist, or a panic.
//
// A failed Get does not land here — gokrb5 falls through to full validation.
// Told apart by the manager's error, else by WWW-Authenticate; never by status.
var ErrSPNEGOHandlerFailed = errors.New("spnego handler reported an internal failure")
