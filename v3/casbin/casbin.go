package casbin

import (
	"fmt"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	utilsstrings "github.com/gofiber/utils/v2/strings"
)

// Middleware holds the enforce and role-lookup functions resolved at
// construction time from a Casbin enforcer, plus the shared handler config.
type Middleware struct {
	lookup       func(fiber.Ctx) string
	unauthorized fiber.Handler
	forbidden    fiber.Handler

	// enforce and getRolesForUser are set by New to call the enforcer without
	// exposing the concrete Casbin type to the request handlers.
	enforce         func(rvals ...any) (bool, error)
	getRolesForUser func(name string, domain ...string) ([]string, error)
}

// New creates an authorization middleware for use in Fiber with Casbin v3.
func New(config ...Config) *Middleware {
	cfg, err := configDefault(config...)
	if err != nil {
		panic(fmt.Errorf("fiber: casbin middleware error -> %w", err))
	}

	return &Middleware{
		// The enforcer may keep the subject, so it must not view a request buffer.
		lookup:          func(c fiber.Ctx) string { return utils.CopyString(cfg.Lookup(c)) },
		unauthorized:    cfg.Unauthorized,
		forbidden:       cfg.Forbidden,
		enforce:         cfg.Enforcer.Enforce,
		getRolesForUser: cfg.Enforcer.GetRolesForUser,
	}
}

// RequiresPermissions tries to find the current subject and determine if the
// subject has the required permissions according to predefined Casbin policies.
func (m *Middleware) RequiresPermissions(permissions []string, opts ...Option) fiber.Handler {
	options := optionsDefault(opts...)

	return func(c fiber.Ctx) error {
		if len(permissions) == 0 {
			return c.Next()
		}

		sub := m.lookup(c)
		if len(sub) == 0 {
			return m.unauthorized(c)
		}

		switch options.ValidationRule {
		case MatchAllRule:
			for _, permission := range permissions {
				vals := append([]string{sub}, options.PermissionParser(permission)...)
				if ok, err := m.enforce(stringSliceToInterfaceSlice(vals)...); err != nil {
					return c.SendStatus(fiber.StatusInternalServerError)
				} else if !ok {
					return m.forbidden(c)
				}
			}
			return c.Next()
		case AtLeastOneRule:
			for _, permission := range permissions {
				vals := append([]string{sub}, options.PermissionParser(permission)...)
				if ok, err := m.enforce(stringSliceToInterfaceSlice(vals)...); err != nil {
					return c.SendStatus(fiber.StatusInternalServerError)
				} else if ok {
					return c.Next()
				}
			}
			return m.forbidden(c)
		}

		return c.Next()
	}
}

// RoutePermission tries to find the current subject and determine if the
// subject has the required permissions according to predefined Casbin policies.
// This method uses http Path and Method as object and action. When the router
// works with other spellings of the path, all of them have to be allowed, and
// so does GET for a HEAD request.
func (m *Middleware) RoutePermission() fiber.Handler {
	return func(c fiber.Ctx) error {
		sub := m.lookup(c)
		if len(sub) == 0 {
			return m.unauthorized(c)
		}

		methods := []string{c.Method()}
		// The router answers HEAD with the handlers of GET routes, so GET has to be allowed too.
		if methods[0] == fiber.MethodHead && !c.App().Config().DisableHeadAutoRegister {
			methods = append(methods, fiber.MethodGet)
		}

		for _, path := range routePaths(c) {
			for _, method := range methods {
				if ok, err := m.enforce(sub, path, method); err != nil {
					return c.SendStatus(fiber.StatusInternalServerError)
				} else if !ok {
					return m.forbidden(c)
				}
			}
		}

		return c.Next()
	}
}

// routePaths returns c.Path() and, where they differ, its slash-trimmed form,
// which route params are cut from, and that case-folded, which routes match on.
func routePaths(c fiber.Ctx) []string {
	// The enforcer may keep the path, so it must not view the pooled path buffer.
	path := utils.CopyString(c.Path())
	cfg := c.App().Config()
	paths := []string{path}

	trimmed := path
	if !cfg.StrictRouting && len(trimmed) > 1 {
		trimmed = utils.TrimRight(trimmed, '/')
		// Slashes alone are the root, as in the router.
		if trimmed == "" {
			trimmed = "/"
		}
	}
	if trimmed != path {
		paths = append(paths, trimmed)
	}

	if !cfg.CaseSensitive {
		if routed := utilsstrings.ToLower(trimmed); routed != trimmed {
			paths = append(paths, routed)
		}
	}
	return paths
}

// RequiresRoles tries to find the current subject and determine if the
// subject has the required roles according to predefined Casbin policies.
func (m *Middleware) RequiresRoles(roles []string, opts ...Option) fiber.Handler {
	options := optionsDefault(opts...)

	return func(c fiber.Ctx) error {
		if len(roles) == 0 {
			return c.Next()
		}

		sub := m.lookup(c)
		if len(sub) == 0 {
			return m.unauthorized(c)
		}

		userRoles, err := m.getRolesForUser(sub)
		if err != nil {
			return c.SendStatus(fiber.StatusInternalServerError)
		}

		switch options.ValidationRule {
		case MatchAllRule:
			for _, role := range roles {
				if !containsString(userRoles, role) {
					return m.forbidden(c)
				}
			}
			return c.Next()
		case AtLeastOneRule:
			for _, role := range roles {
				if containsString(userRoles, role) {
					return c.Next()
				}
			}
			return m.forbidden(c)
		}

		return c.Next()
	}
}
