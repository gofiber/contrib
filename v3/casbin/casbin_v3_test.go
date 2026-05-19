package casbin

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	casbinv3 "github.com/casbin/casbin/v3"
	casbinv3model "github.com/casbin/casbin/v3/model"
	casbinv3persist "github.com/casbin/casbin/v3/persist"
	"github.com/gofiber/fiber/v3"
)

// mockAdapterV3 is a v3-compatible in-memory policy adapter for testing.
type mockAdapterV3 struct {
	text string
}

func newMockAdapterV3(text string) *mockAdapterV3 {
	return &mockAdapterV3{text: text}
}

func (ma *mockAdapterV3) LoadPolicy(model casbinv3model.Model) error {
	if ma.text == "" {
		return errors.New("text is required")
	}

	for _, str := range strings.Split(ma.text, "\n") {
		if str == "" {
			continue
		}
		if err := casbinv3persist.LoadPolicyLine(str, model); err != nil {
			return err
		}
	}

	return nil
}

func (ma *mockAdapterV3) SavePolicy(model casbinv3model.Model) error {
	return errors.New("not implemented")
}

func (ma *mockAdapterV3) AddPolicy(sec string, ptype string, rule []string) error {
	return errors.New("not implemented")
}

func (ma *mockAdapterV3) RemovePolicy(sec string, ptype string, rule []string) error {
	return errors.New("not implemented")
}

func (ma *mockAdapterV3) RemoveFilteredPolicy(sec string, ptype string, fieldIndex int, fieldValues ...string) error {
	return errors.New("not implemented")
}

func setupV3() (*casbinv3.Enforcer, error) {
	m, err := casbinv3model.NewModelFromString(modelConf)
	if err != nil {
		return nil, err
	}

	enf, err := casbinv3.NewEnforcer(m, newMockAdapterV3(policyList))
	if err != nil {
		return nil, err
	}

	return enf, nil
}

func Test_RequiresPermission_V3(t *testing.T) {
	enf, err := setupV3()
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		desc        string
		lookup      func(fiber.Ctx) string
		permissions []string
		opts        []Option
		statusCode  int
	}{
		{
			desc:        "alice has permission to create blog",
			lookup:      subjectAlice,
			permissions: []string{"blog:create"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  200,
		},
		{
			desc:        "alice has permission to create blog",
			lookup:      subjectAlice,
			permissions: []string{"blog:create"},
			opts:        []Option{WithValidationRule(AtLeastOneRule)},
			statusCode:  200,
		},
		{
			desc:        "alice has permission to create and update blog",
			lookup:      subjectAlice,
			permissions: []string{"blog:create", "blog:update"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  200,
		},
		{
			desc:        "alice has permission to create comment or blog",
			lookup:      subjectAlice,
			permissions: []string{"comment:create", "blog:create"},
			opts:        []Option{WithValidationRule(AtLeastOneRule)},
			statusCode:  200,
		},
		{
			desc:        "bob has only permission to create comment",
			lookup:      subjectBob,
			permissions: []string{"comment:create", "blog:create"},
			opts:        []Option{WithValidationRule(AtLeastOneRule)},
			statusCode:  200,
		},
		{
			desc:        "unauthenticated user has no permissions",
			lookup:      subjectEmpty,
			permissions: []string{"comment:create"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  401,
		},
		{
			desc:        "bob has not permission to create blog",
			lookup:      subjectBob,
			permissions: []string{"blog:create"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  403,
		},
		{
			desc:        "bob has not permission to delete blog",
			lookup:      subjectBob,
			permissions: []string{"blog:delete"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  403,
		},
		{
			desc:        "invalid permission",
			lookup:      subjectBob,
			permissions: []string{"unknown"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  500,
		},
	}

	for _, tC := range testCases {
		app := fiber.New()

		authz := NewV3(ConfigV3{
			Enforcer: enf,
			Lookup:   tC.lookup,
		})

		app.Post("/blog",
			authz.RequiresPermissions(tC.permissions, tC.opts...),
			func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			},
		)

		t.Run(tC.desc, func(t *testing.T) {
			req, err := http.NewRequest("POST", "/blog", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "localhost"

			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != tC.statusCode {
				t.Errorf(`StatusCode: got %v - expected %v`, resp.StatusCode, tC.statusCode)
			}
		})
	}
}

func Test_RequiresRoles_V3(t *testing.T) {
	enf, err := setupV3()
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		desc       string
		lookup     func(fiber.Ctx) string
		roles      []string
		opts       []Option
		statusCode int
	}{
		{
			desc:       "alice has user role",
			lookup:     subjectAlice,
			roles:      []string{"user"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 200,
		},
		{
			desc:       "alice has admin role",
			lookup:     subjectAlice,
			roles:      []string{"admin"},
			opts:       []Option{WithValidationRule(AtLeastOneRule)},
			statusCode: 200,
		},
		{
			desc:       "alice has both user and admin roles",
			lookup:     subjectAlice,
			roles:      []string{"user", "admin"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 200,
		},
		{
			desc:       "alice has both user and admin roles",
			lookup:     subjectAlice,
			roles:      []string{"user", "admin"},
			opts:       []Option{WithValidationRule(AtLeastOneRule)},
			statusCode: 200,
		},
		{
			desc:       "bob has only user role",
			lookup:     subjectBob,
			roles:      []string{"user"},
			opts:       []Option{WithValidationRule(AtLeastOneRule)},
			statusCode: 200,
		},
		{
			desc:       "unauthenticated user has no permissions",
			lookup:     subjectEmpty,
			roles:      []string{"user"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 401,
		},
		{
			desc:       "bob has not admin role",
			lookup:     subjectBob,
			roles:      []string{"admin"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 403,
		},
		{
			desc:       "bob has only user role",
			lookup:     subjectBob,
			roles:      []string{"admin", "user"},
			opts:       []Option{WithValidationRule(AtLeastOneRule)},
			statusCode: 200,
		},
		{
			desc:       "invalid role",
			lookup:     subjectBob,
			roles:      []string{"unknown"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 403,
		},
	}

	for _, tC := range testCases {
		app := fiber.New()

		authz := NewV3(ConfigV3{
			Enforcer: enf,
			Lookup:   tC.lookup,
		})

		app.Post("/blog",
			authz.RequiresRoles(tC.roles, tC.opts...),
			func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			},
		)

		t.Run(tC.desc, func(t *testing.T) {
			req, err := http.NewRequest("POST", "/blog", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "localhost"

			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != tC.statusCode {
				t.Errorf(`StatusCode: got %v - expected %v`, resp.StatusCode, tC.statusCode)
			}
		})
	}
}

func Test_RoutePermission_V3(t *testing.T) {
	enf, err := setupV3()
	if err != nil {
		t.Fatal(err)
	}

	testCases := []struct {
		desc       string
		url        string
		method     string
		subject    string
		statusCode int
	}{
		{
			desc:       "alice has permission to create blog",
			url:        "/blog",
			method:     "POST",
			subject:    "alice",
			statusCode: 200,
		},
		{
			desc:       "alice has permission to update blog",
			url:        "/blog/1",
			method:     "PUT",
			subject:    "alice",
			statusCode: 200,
		},
		{
			desc:       "bob has only permission to create comment",
			url:        "/comment",
			method:     "POST",
			subject:    "bob",
			statusCode: 200,
		},
		{
			desc:       "unauthenticated user has no permissions",
			url:        "/",
			method:     "POST",
			subject:    "",
			statusCode: 401,
		},
		{
			desc:       "bob has not permission to create blog",
			url:        "/blog",
			method:     "POST",
			subject:    "bob",
			statusCode: 403,
		},
		{
			desc:       "bob has not permission to delete blog",
			url:        "/blog/1",
			method:     "DELETE",
			subject:    "bob",
			statusCode: 403,
		},
	}

	app := fiber.New()

	authz := NewV3(ConfigV3{
		Enforcer: enf,
		Lookup: func(c fiber.Ctx) string {
			return c.Get("x-subject")
		},
	})

	app.Use(authz.RoutePermission())

	app.Post("/blog",
		func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Put("/blog/:id",
		func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Delete("/blog/:id",
		func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Post("/comment",
		func(c fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)

	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			req, err := http.NewRequest(tC.method, tC.url, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "localhost"

			req.Header.Set("x-subject", tC.subject)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatal(err)
			}

			if resp.StatusCode != tC.statusCode {
				t.Errorf(`StatusCode: got %v - expected %v`, resp.StatusCode, tC.statusCode)
			}
		})
	}
}
