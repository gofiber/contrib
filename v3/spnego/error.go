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

// errNilKeytab is returned when a KeytabLookupFunc reports success but yields
// no keytab. It is internal: callers see ErrLookupKeytabFailed.
var errNilKeytab = errors.New("keytab lookup returned no keytab")
