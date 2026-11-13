package casbin

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	"github.com/gofiber/fiber/v3"
)

var (
	subjectAlice = func(c fiber.Ctx) string { return "alice" }
	subjectBob   = func(c fiber.Ctx) string { return "bob" }
	subjectEmpty = func(c fiber.Ctx) string { return "" }
)

const (
	modelConf = `
	[request_definition]
	r = sub, obj, act

	[policy_definition]
	p = sub, obj, act

	[role_definition]
	g = _, _

	[policy_effect]
	e = some(where (p.eft == allow))

	[matchers]
	m = g(r.sub, p.sub) && r.obj == p.obj && r.act == p.act`

	policyList = `
	p,admin,blog,create
	p,admin,blog,update
	p,admin,blog,delete
	p,user,comment,create
	p,user,comment,delete

	p,admin,/blog,POST
	p,admin,/blog/1,PUT
	p,admin,/blog/1,DELETE
	p,user,/comment,POST


	g,alice,admin
	g,alice,user
	g,bob,user`
)

// mockAdapter is an in-memory policy adapter for testing.
type mockAdapter struct {
	text string
}

func newMockAdapter(text string) *mockAdapter {
	return &mockAdapter{text: text}
}

func (ma *mockAdapter) LoadPolicy(model model.Model) error {
	if ma.text == "" {
		return errors.New("text is required")
	}

	for _, str := range strings.Split(ma.text, "\n") {
		if str == "" {
			continue
		}
		if err := persist.LoadPolicyLine(str, model); err != nil {
			return err
		}
	}

	return nil
}

func (ma *mockAdapter) SavePolicy(model model.Model) error {
	return errors.New("not implemented")
}

func (ma *mockAdapter) AddPolicy(sec string, ptype string, rule []string) error {
	return errors.New("not implemented")
}

func (ma *mockAdapter) RemovePolicy(sec string, ptype string, rule []string) error {
	return errors.New("not implemented")
}

func (ma *mockAdapter) RemoveFilteredPolicy(sec string, ptype string, fieldIndex int, fieldValues ...string) error {
	return errors.New("not implemented")
}

func setup() (*casbin.Enforcer, error) {
	m, err := model.NewModelFromString(modelConf)
	if err != nil {
		return nil, err
	}

	enf, err := casbin.NewEnforcer(m, newMockAdapter(policyList))
	if err != nil {
		return nil, err
	}

	return enf, nil
}

func Test_RequiresPermission(t *testing.T) {
	enf, err := setup()
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

		authz := New(Config{
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

func Test_RequiresRoles(t *testing.T) {
	enf, err := setup()
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

		authz := New(Config{
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

func Test_RoutePermission(t *testing.T) {
	enf, err := setup()
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

	authz := New(Config{
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

// The router folds case and trailing slashes while params keep the client's spelling, so both must be allowed.
func Test_RoutePermission_RouterSpelling(t *testing.T) {
	const restModel = `
	[request_definition]
	r = sub, obj, act

	[policy_definition]
	p = sub, obj, act, eft

	[policy_effect]
	e = some(where (p.eft == allow)) && !some(where (p.eft == deny))

	[matchers]
	m = r.sub == p.sub && keyMatch(r.obj, p.obj) && regexMatch(r.act, p.act)`

	const restPolicy = `
	p,bob,/*,GET,allow
	p,bob,/admin,GET,deny
	p,bob,/admin/*,GET,deny
	p,bob,/blog/*,DELETE,allow
	p,bob,/doc/abc,PUT,allow`

	testCases := []struct {
		desc       string
		config     fiber.Config
		url        string
		method     string
		statusCode int
	}{
		{desc: "denied path", url: "/admin", method: "GET", statusCode: 403},
		{desc: "denied path in upper case", url: "/ADMIN", method: "GET", statusCode: 403},
		{desc: "denied path in escaped upper case", url: "/%41dmin", method: "GET", statusCode: 403},
		{desc: "denied sub path in mixed case", url: "/Admin/users", method: "GET", statusCode: 403},
		{desc: "collection is not covered by the item pattern", url: "/blog", method: "DELETE", statusCode: 403},
		{desc: "collection with a trailing slash", url: "/blog/", method: "DELETE", statusCode: 403},
		{desc: "collection with trailing slashes", url: "/blog//", method: "DELETE", statusCode: 403},
		{desc: "allowed item", url: "/blog/1", method: "DELETE", statusCode: 200},
		{desc: "allowed item with a trailing slash", url: "/blog/1/", method: "DELETE", statusCode: 200},
		{desc: "allowed path", url: "/blog", method: "GET", statusCode: 200},
		{desc: "allowed path in upper case", url: "/BLOG/", method: "GET", statusCode: 200},
		{desc: "allowed parameter", url: "/doc/abc", method: "PUT", statusCode: 200},
		{desc: "another parameter that folds to the allowed one", url: "/doc/ABC", method: "PUT", statusCode: 403},
		{desc: "case sensitive router", config: fiber.Config{CaseSensitive: true}, url: "/ADMIN", method: "GET", statusCode: 404},
		{desc: "strict router", config: fiber.Config{StrictRouting: true}, url: "/blog/", method: "DELETE", statusCode: 404},
	}

	m, err := model.NewModelFromString(restModel)
	if err != nil {
		t.Fatal(err)
	}

	enf, err := casbin.NewEnforcer(m, newMockAdapter(restPolicy))
	if err != nil {
		t.Fatal(err)
	}

	authz := New(Config{
		Enforcer: enf,
		Lookup:   subjectBob,
	})

	ok := func(c fiber.Ctx) error {
		return c.SendStatus(fiber.StatusOK)
	}

	for _, tC := range testCases {
		app := fiber.New(tC.config)
		app.Use(authz.RoutePermission())
		app.Get("/admin", ok)
		app.Get("/admin/users", ok)
		app.Get("/blog", ok)
		app.Delete("/blog", ok)
		app.Delete("/blog/:id", ok)
		app.Put("/doc/:id", ok)

		t.Run(tC.desc, func(t *testing.T) {
			req, err := http.NewRequest(tC.method, tC.url, nil)
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

func Test_EnforcerImplementations(t *testing.T) {
	newModel := func(t *testing.T) model.Model {
		t.Helper()
		m, err := model.NewModelFromString(modelConf)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}

	enforcers := []struct {
		desc string
		new  func(t *testing.T) (casbin.IEnforcer, error)
	}{
		{"Enforcer", func(t *testing.T) (casbin.IEnforcer, error) {
			return casbin.NewEnforcer(newModel(t), newMockAdapter(policyList))
		}},
		{"SyncedEnforcer", func(t *testing.T) (casbin.IEnforcer, error) {
			return casbin.NewSyncedEnforcer(newModel(t), newMockAdapter(policyList))
		}},
		{"CachedEnforcer", func(t *testing.T) (casbin.IEnforcer, error) {
			return casbin.NewCachedEnforcer(newModel(t), newMockAdapter(policyList))
		}},
		{"DistributedEnforcer", func(t *testing.T) (casbin.IEnforcer, error) {
			return casbin.NewDistributedEnforcer(newModel(t), newMockAdapter(policyList))
		}},
		{"SyncedCachedEnforcer", func(t *testing.T) (casbin.IEnforcer, error) {
			return casbin.NewSyncedCachedEnforcer(newModel(t), newMockAdapter(policyList))
		}},
	}

	testCases := []struct {
		desc       string
		lookup     func(fiber.Ctx) string
		handler    func(*Middleware) fiber.Handler
		statusCode int
	}{
		{"alice may create blog", subjectAlice, func(m *Middleware) fiber.Handler { return m.RequiresPermissions([]string{"blog:create"}) }, 200},
		{"bob may not create blog", subjectBob, func(m *Middleware) fiber.Handler { return m.RequiresPermissions([]string{"blog:create"}) }, 403},
		{"empty subject is unauthorized", subjectEmpty, func(m *Middleware) fiber.Handler { return m.RequiresPermissions([]string{"blog:create"}) }, 401},
		{"alice has admin role", subjectAlice, func(m *Middleware) fiber.Handler { return m.RequiresRoles([]string{"admin"}) }, 200},
		{"bob lacks admin role", subjectBob, func(m *Middleware) fiber.Handler { return m.RequiresRoles([]string{"admin"}) }, 403},
		{"alice route permission allowed", subjectAlice, func(m *Middleware) fiber.Handler { return m.RoutePermission() }, 200},
		{"bob route permission forbidden", subjectBob, func(m *Middleware) fiber.Handler { return m.RoutePermission() }, 403},
	}

	for _, e := range enforcers {
		enf, err := e.new(t)
		if err != nil {
			t.Fatal(err)
		}

		for _, tC := range testCases {
			t.Run(e.desc+"/"+tC.desc, func(t *testing.T) {
				authz := New(Config{Enforcer: enf, Lookup: tC.lookup})

				app := fiber.New()
				app.Post("/blog", tC.handler(authz), func(c fiber.Ctx) error {
					return c.SendStatus(fiber.StatusOK)
				})

				req, err := http.NewRequest(http.MethodPost, "/blog", nil)
				if err != nil {
					t.Fatal(err)
				}
				req.Host = "localhost"

				resp, err := app.Test(req)
				if err != nil {
					t.Fatal(err)
				}

				if resp.StatusCode != tC.statusCode {
					t.Errorf("StatusCode: got %v - expected %v", resp.StatusCode, tC.statusCode)
				}
			})
		}
	}
}
