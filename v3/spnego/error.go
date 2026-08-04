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

// errSPNEGOInternal marks a 5xx that gokrb5 produced inside the SPNEGO handler
// rather than one this middleware raised — today, a session manager that could
// not store or retrieve a session. It is internal: it exists so those failures
// reach the log and Config.OnError instead of passing as an ordinary response.
var errSPNEGOInternal = errors.New("spnego handler reported an internal failure")
