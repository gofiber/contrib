package casbin

import (
	"fmt"

	"github.com/gofiber/fiber/v2"
)

// Middleware ...
type Middleware struct {
	config Config
}

// New creates an authorization middleware for use in Fiber
func New(config ...Config) *Middleware {
	cfg, err := configDefault(config...)
	if err != nil {
		panic(fmt.Errorf("Fiber: casbin middleware error -> %w", err))
	}

	return &Middleware{
		config: cfg,
	}
}

// RequiresPermissions tries to find the current subject and determine if the
// subject has the required permissions according to predefined Casbin policies.
func (m *Middleware) RequiresPermissions(permissions []string, opts ...Option) fiber.Handler {
	options := optionsDefault(opts...)

	return func(c *fiber.Ctx) error {
		if len(permissions) == 0 {
			return c.Next()
		}

		sub := m.config.Lookup(c)
		if len(sub) == 0 {
			return m.config.Unauthorized(c)
		}

		if options.ValidationRule == MatchAllRule {
			for _, permission := range permissions {
				vals := append([]string{sub}, options.PermissionParser(permission)...)
				if ok, err := m.config.Enforcer.Enforce(stringSliceToInterfaceSlice(vals)...); err != nil {
					return c.SendStatus(fiber.StatusInternalServerError)
				} else if !ok {
					return m.config.Forbidden(c)
				}
			}
			return c.Next()
		} else if options.ValidationRule == AtLeastOneRule {
			for _, permission := range permissions {
				vals := append([]string{sub}, options.PermissionParser(permission)...)
				if ok, err := m.config.Enforcer.Enforce(stringSliceToInterfaceSlice(vals)...); err != nil {
					return c.SendStatus(fiber.StatusInternalServerError)
				} else if ok {
					return c.Next()
				}
			}
			return m.config.Forbidden(c)
		}

		return c.Next()
	}
}

// RoutePermission tries to find the current subject and determine if the
// subject has the required permissions according to predefined Casbin policies.
// This method uses http Path and Method as object and action.
func (m *Middleware) RoutePermission() fiber.Handler {
	return func(c *fiber.Ctx) error {
		sub := m.config.Lookup(c)
		if len(sub) == 0 {
			return m.config.Unauthorized(c)
		}

		if ok, err := m.config.Enforcer.Enforce(sub, c.Path(), c.Method()); err != nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		} else if !ok {
			return m.config.Forbidden(c)
		}

		return c.Next()
	}
}

// RequiresRoles tries to find the current subject and determine if the
// subject has the required roles according to predefined Casbin policies.
func (m *Middleware) RequiresRoles(roles []string, opts ...Option) fiber.Handler {
	options := optionsDefault(opts...)

	return func(c *fiber.Ctx) error {
		if len(roles) == 0 {
			return c.Next()
		}

		sub := m.config.Lookup(c)
		if len(sub) == 0 {
			return m.config.Unauthorized(c)
		}

		userRoles, err := m.config.Enforcer.GetRolesForUser(sub)
		if err != nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}

		if options.ValidationRule == MatchAllRule {
			for _, role := range roles {
				if !containsString(userRoles, role) {
					return m.config.Forbidden(c)
				}
			}
			return c.Next()
		} else if options.ValidationRule == AtLeastOneRule {
			for _, role := range roles {
				if containsString(userRoles, role) {
					return c.Next()
				}
			}
			return m.config.Forbidden(c)
		}

		return c.Next()
	}
}
