package spnego

import "errors"

// ErrConfigInvalidOfKeytabLookupFunctionRequired is returned when the KeytabLookup function is not set in Config
var ErrConfigInvalidOfKeytabLookupFunctionRequired = errors.New("config invalid: keytab lookup function is required")

// ErrLookupKeytabFailed is returned when the keytab lookup fails
var ErrLookupKeytabFailed = errors.New("keytab lookup failed")

// ErrConvertRequestFailed is returned when the request conversion to HTTP request fails
var ErrConvertRequestFailed = errors.New("convert request failed")

// ErrConfigInvalidOfAtLeastOneKeytabFileRequired is returned when no keytab files are provided
var ErrConfigInvalidOfAtLeastOneKeytabFileRequired = errors.New("config invalid: at least one keytab file required")

// ErrLoadKeytabFileFailed is returned when load keytab files failed
var ErrLoadKeytabFileFailed = errors.New("load keytab failed")
