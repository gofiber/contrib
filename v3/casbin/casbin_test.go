package casbin

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/casbin/casbin/v3"
	"github.com/casbin/casbin/v3/model"
	"github.com/casbin/casbin/v3/persist"
	"github.com/gofiber/fiber/v3"
	"github.com/valyala/fasthttp"
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
	p,bob,*,GET,allow
	p,bob,/,GET,deny
	p,bob,/admin,GET,deny
	p,bob,/admin/*,GET,deny
	p,bob,/blog/*,DELETE,allow
	p,bob,/doc/abc,PUT,allow
	p,bob,/doc/ABC/*,PUT,allow`

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
		{desc: "denied root spelled with slashes only", url: "///", method: "GET", statusCode: 403},
		{desc: "denied root behind a dot segment", url: "/.//", method: "GET", statusCode: 403},
		{desc: "collection is not covered by the item pattern", url: "/blog", method: "DELETE", statusCode: 403},
		{desc: "collection with a trailing slash", url: "/blog/", method: "DELETE", statusCode: 403},
		{desc: "collection with trailing slashes", url: "/blog//", method: "DELETE", statusCode: 403},
		{desc: "allowed item", url: "/blog/1", method: "DELETE", statusCode: 200},
		{desc: "allowed item with a trailing slash", url: "/blog/1/", method: "DELETE", statusCode: 200},
		{desc: "allowed path", url: "/blog", method: "GET", statusCode: 200},
		{desc: "allowed path in upper case", url: "/BLOG/", method: "GET", statusCode: 200},
		{desc: "allowed parameter", url: "/doc/abc", method: "PUT", statusCode: 200},
		{desc: "another parameter that folds to the allowed one", url: "/doc/ABC", method: "PUT", statusCode: 403},
		{desc: "another parameter that folds to the allowed one, with a trailing slash", url: "/doc/ABC/", method: "PUT", statusCode: 403},
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
		app.Get("/", ok)
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

func Test_RoutePermission_AutoHead(t *testing.T) {
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
	p,bob,/reports/*,.*,allow
	p,bob,/reports/export,GET,deny
	p,bob,/reports/archive,HEAD,deny
	p,bob,/reports/probe,GET,deny`

	testCases := []struct {
		desc       string
		config     fiber.Config
		url        string
		method     string
		statusCode int
	}{
		{desc: "GET denied", url: "/reports/export", method: "GET", statusCode: 403},
		{desc: "HEAD on a path denied for GET", url: "/reports/export", method: "HEAD", statusCode: 403},
		{desc: "HEAD on a path denied for GET, with a trailing slash", url: "/reports/export/", method: "HEAD", statusCode: 403},
		{desc: "HEAD on a path allowed for GET", url: "/reports/summary", method: "HEAD", statusCode: 200},
		{desc: "HEAD denied", url: "/reports/archive", method: "HEAD", statusCode: 403},
		{desc: "GET on a path denied for HEAD", url: "/reports/archive", method: "GET", statusCode: 200},
		{desc: "own HEAD route on a path denied for GET", url: "/reports/probe", method: "HEAD", statusCode: 403},
		{desc: "no automatic HEAD routes", config: fiber.Config{DisableHeadAutoRegister: true}, url: "/reports/export", method: "HEAD", statusCode: 405},
		{desc: "own HEAD route without automatic HEAD routes", config: fiber.Config{DisableHeadAutoRegister: true}, url: "/reports/probe", method: "HEAD", statusCode: 200},
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
		app.Get("/reports/export", authz.RoutePermission(), ok)
		app.Get("/reports/summary", authz.RoutePermission(), ok)
		app.Get("/reports/archive", authz.RoutePermission(), ok)
		app.Get("/reports/probe", authz.RoutePermission(), ok)
		app.Head("/reports/probe", authz.RoutePermission(), ok)

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

type retainingEnforcer struct {
	casbin.IEnforcer
	subjects []string
	objects  []string
}

func (e *retainingEnforcer) Enforce(rvals ...any) (bool, error) {
	e.subjects = append(e.subjects, rvals[0].(string))
	e.objects = append(e.objects, rvals[1].(string))
	return true, nil
}

func (e *retainingEnforcer) GetRolesForUser(name string, _ ...string) ([]string, error) {
	e.subjects = append(e.subjects, name)
	return []string{"admin"}, nil
}

func Test_EnforcerGetsCopiesOfRequestStrings(t *testing.T) {
	testCases := []struct {
		desc    string
		handler func(*Middleware) fiber.Handler
		objects string
	}{
		{"RoutePermission", func(m *Middleware) fiber.Handler { return m.RoutePermission() }, "/first"},
		{"RequiresPermissions", func(m *Middleware) fiber.Handler { return m.RequiresPermissions([]string{"blog:create"}) }, "blog"},
		{"RequiresRoles", func(m *Middleware) fiber.Handler { return m.RequiresRoles([]string{"admin"}) }, ""},
	}

	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			fctx := &fasthttp.RequestCtx{}
			for range 50 {
				enf := &retainingEnforcer{}
				authz := New(Config{
					Enforcer: enf,
					Lookup:   func(c fiber.Ctx) string { return c.Get("X-User") },
				})

				app := fiber.New()
				app.Get("/:name", tC.handler(authz), func(c fiber.Ctx) error {
					return c.SendStatus(fiber.StatusOK)
				})
				h := app.Handler()

				for _, r := range []struct{ path, user string }{{"/first", "alice"}, {"/other", "admin"}} {
					fctx.Request.Reset()
					fctx.Request.SetRequestURI(r.path)
					fctx.Request.Header.Set("X-User", r.user)
					h(fctx)

					if code := fctx.Response.StatusCode(); code != fiber.StatusOK {
						t.Fatalf("StatusCode: got %v - expected %v", code, fiber.StatusOK)
					}
				}

				var object string
				if len(enf.objects) > 0 {
					object = enf.objects[0]
				}
				if enf.subjects[0] != "alice" || object != tC.objects {
					t.Fatalf("first request's strings after the next request: got %q, %q - expected %q, %q", enf.subjects[0], object, "alice", tC.objects)
				}
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

func Test_NilEnforcerPointerUsesDefault(t *testing.T) {
	modelPath := filepath.Join(t.TempDir(), "model.conf")
	if err := os.WriteFile(modelPath, []byte(modelConf), 0o600); err != nil {
		t.Fatal(err)
	}

	var enf *casbin.Enforcer
	authz := New(Config{
		Enforcer:      enf,
		ModelFilePath: modelPath,
		PolicyAdapter: newMockAdapter(policyList),
		Lookup:        subjectAlice,
	})

	app := fiber.New()
	app.Post("/blog", authz.RequiresRoles([]string{"admin"}), func(c fiber.Ctx) error {
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

	if resp.StatusCode != fiber.StatusOK {
		t.Errorf("StatusCode: got %v - expected %v", resp.StatusCode, fiber.StatusOK)
	}
}

func Test_NewWithoutConfigUsesDefaultFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "model.conf"), []byte(modelConf), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy.csv"), []byte(policyList), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	authz := New()

	if ok, err := authz.enforce("alice", "blog", "create"); err != nil || !ok {
		t.Errorf("alice blog:create: got %v, %v - expected true, nil", ok, err)
	}
	if ok, err := authz.enforce("bob", "blog", "create"); err != nil || ok {
		t.Errorf("bob blog:create: got %v, %v - expected false, nil", ok, err)
	}
}
