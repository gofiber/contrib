package pydanticai

import "time"

// WithStreamEndGrace returns cfg with the time that a write to a client that
// stopped reading may block, once the run was told to end, set to grace.
func WithStreamEndGrace[Deps, A any](cfg StreamConfig[Deps, A], grace time.Duration) StreamConfig[Deps, A] {
	cfg.grace = grace
	return cfg
}
