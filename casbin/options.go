package casbin

import "strings"

const (
	MatchAllRule ValidationRule = iota
	AtLeastOneRule
)

var OptionsDefault = Options{
	ValidationRule:   MatchAllRule,
	PermissionParser: PermissionParserWithSeperator(":"),
}

type (
	ValidationRule int
	// PermissionParserFunc is used for parsing the permission
	// to extract object and action usually
	PermissionParserFunc func(str string) []string
	OptionFunc           func(*Options)
	// Option specifies casbin configuration options.
	Option interface {
		apply(*Options)
	}
	// Options holds Options of middleware
	Options struct {
		ValidationRule   ValidationRule
		PermissionParser PermissionParserFunc
	}
)

func (of OptionFunc) apply(o *Options) {
	of(o)
}

func WithValidationRule(vr ValidationRule) Option {
	return OptionFunc(func(o *Options) {
		o.ValidationRule = vr
	})
}

func WithPermissionParser(pp PermissionParserFunc) Option {
	return OptionFunc(func(o *Options) {
		o.PermissionParser = pp
	})
}

func PermissionParserWithSeperator(sep string) PermissionParserFunc {
	return func(str string) []string {
		return strings.Split(str, sep)
	}
}

// Helper function to set default values
func optionsDefault(opts ...Option) Options {
	cfg := OptionsDefault

	for _, opt := range opts {
		opt.apply(&cfg)
	}

	return cfg
}
