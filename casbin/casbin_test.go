package casbin

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/casbin/casbin/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/casbin/casbin/v2/model"
	"github.com/casbin/casbin/v2/persist"
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

// mockAdapter
type mockAdapter struct {
	text string
}

func newMockAdapter(text string) *mockAdapter {
	return &mockAdapter{
		text: text,
	}
}

func (ma *mockAdapter) LoadPolicy(model model.Model) error {
	if ma.text == "" {
		return errors.New("text is required")
	}
	strs := strings.Split(ma.text, "\n")

	for _, str := range strs {
		if str == "" {
			continue
		}
		_ = persist.LoadPolicyLine(str, model)
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

func TestRequiresPermission(t *testing.T) {
	enf, err := setup()
	require.NoError(t, err)

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
			func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			},
			authz.RequiresPermissions(tC.permissions, tC.opts...),
		)

		t.Run(tC.desc, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/blog", nil))
			require.NoError(t, err)
			assert.Equal(t, tC.statusCode, resp.StatusCode)
		})
	}
}

func TestRequiresRoles(t *testing.T) {
	enf, err := setup()
	require.NoError(t, err)

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
			func(c fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			},
			authz.RequiresRoles(tC.roles, tC.opts...),
		)

		t.Run(tC.desc, func(t *testing.T) {
			resp, err := app.Test(httptest.NewRequest(http.MethodPost, "/blog", nil))
			require.NoError(t, err)
			assert.Equal(t, tC.statusCode, resp.StatusCode)
		})
	}
}

func TestRoutePermission(t *testing.T) {
	enf, err := setup()
	require.NoError(t, err)

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
			req := httptest.NewRequest(tC.method, tC.url, nil)
			req.Header.Set("x-subject", tC.subject)
			resp, err := app.Test(req)
			require.NoError(t, err)
			assert.Equal(t, tC.statusCode, resp.StatusCode)
		})
	}
}
