package casbin

import (
	"log"
	"net/http"
	"testing"

	"github.com/casbin/casbin/v2"

	fileadapter "github.com/casbin/casbin/v2/persist/file-adapter"
	"github.com/gofiber/fiber/v2"
)

var (
	subjectAlice = func(c *fiber.Ctx) string { return "alice" }
	subjectBob   = func(c *fiber.Ctx) string { return "bob" }
	subjectNil   = func(c *fiber.Ctx) string { return "" }
)

func Test_RequiresPermission(t *testing.T) {
	tests := []struct {
		name        string
		lookup      func(*fiber.Ctx) string
		permissions []string
		opts        []Option
		statusCode  int
	}{
		{
			name:        "alice has permission to create blog",
			lookup:      subjectAlice,
			permissions: []string{"blog:create"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  200,
		},
		{
			name:        "alice has permission to create blog",
			lookup:      subjectAlice,
			permissions: []string{"blog:create"},
			opts:        []Option{WithValidationRule(AtLeastOneRule)},
			statusCode:  200,
		},
		{
			name:        "alice has permission to create and update blog",
			lookup:      subjectAlice,
			permissions: []string{"blog:create", "blog:update"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  200,
		},
		{
			name:        "alice has permission to create comment or blog",
			lookup:      subjectAlice,
			permissions: []string{"comment:create", "blog:create"},
			opts:        []Option{WithValidationRule(AtLeastOneRule)},
			statusCode:  200,
		},
		{
			name:        "bob has only permission to create comment",
			lookup:      subjectBob,
			permissions: []string{"comment:create", "blog:create"},
			opts:        []Option{WithValidationRule(AtLeastOneRule)},
			statusCode:  200,
		},
		{
			name:        "unauthenticated user has no permissions",
			lookup:      subjectNil,
			permissions: []string{"comment:create"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  401,
		},
		{
			name:        "bob has not permission to create blog",
			lookup:      subjectBob,
			permissions: []string{"blog:create"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  403,
		},
		{
			name:        "bob has not permission to delete blog",
			lookup:      subjectBob,
			permissions: []string{"blog:delete"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  403,
		},
		{
			name:        "invalid permission",
			lookup:      subjectBob,
			permissions: []string{"unknown"},
			opts:        []Option{WithValidationRule(MatchAllRule)},
			statusCode:  500,
		},
	}

	for _, tt := range tests {
		app := *fiber.New()
		authz := New(Config{
			ModelFilePath: "./example/model.conf",
			PolicyAdapter: fileadapter.NewAdapter("./example/policy.csv"),
			Lookup:        tt.lookup,
		})

		app.Post("/blog",
			authz.RequiresPermissions(tt.permissions, tt.opts...),
			func(c *fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			},
		)

		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "/blog", nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf(`%s: %s`, t.Name(), err)
			}

			if resp.StatusCode != tt.statusCode {
				t.Fatalf(`%s: StatusCode: got %v - expected %v`, t.Name(), resp.StatusCode, tt.statusCode)
			}
		})
	}
}

func Test_RequiresRoles(t *testing.T) {
	tests := []struct {
		name       string
		lookup     func(*fiber.Ctx) string
		roles      []string
		opts       []Option
		statusCode int
	}{
		{
			name:       "alice has user role",
			lookup:     subjectAlice,
			roles:      []string{"user"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 200,
		},
		{
			name:       "alice has admin role",
			lookup:     subjectAlice,
			roles:      []string{"admin"},
			opts:       []Option{WithValidationRule(AtLeastOneRule)},
			statusCode: 200,
		},
		{
			name:       "alice has both user and admin roles",
			lookup:     subjectAlice,
			roles:      []string{"user", "admin"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 200,
		},
		{
			name:       "alice has both user and admin roles",
			lookup:     subjectAlice,
			roles:      []string{"user", "admin"},
			opts:       []Option{WithValidationRule(AtLeastOneRule)},
			statusCode: 200,
		},
		{
			name:       "bob has only user role",
			lookup:     subjectBob,
			roles:      []string{"user"},
			opts:       []Option{WithValidationRule(AtLeastOneRule)},
			statusCode: 200,
		},
		{
			name:       "unauthenticated user has no permissions",
			lookup:     subjectNil,
			roles:      []string{"user"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 401,
		},
		{
			name:       "bob has not admin role",
			lookup:     subjectBob,
			roles:      []string{"admin"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 403,
		},
		{
			name:       "bob has only user role",
			lookup:     subjectBob,
			roles:      []string{"admin", "user"},
			opts:       []Option{WithValidationRule(AtLeastOneRule)},
			statusCode: 200,
		},
		{
			name:       "invalid role",
			lookup:     subjectBob,
			roles:      []string{"unknown"},
			opts:       []Option{WithValidationRule(MatchAllRule)},
			statusCode: 403,
		},
	}

	for _, tt := range tests {
		app := *fiber.New()
		authz := New(Config{
			ModelFilePath: "./example/model.conf",
			PolicyAdapter: fileadapter.NewAdapter("./example/policy.csv"),
			Lookup:        tt.lookup,
		})

		app.Post("/blog",
			authz.RequiresRoles(tt.roles, tt.opts...),
			func(c *fiber.Ctx) error {
				return c.SendStatus(fiber.StatusOK)
			},
		)

		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", "/blog", nil)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf(`%s: %s`, t.Name(), err)
			}

			if resp.StatusCode != tt.statusCode {
				t.Fatalf(`%s: StatusCode: got %v - expected %v`, t.Name(), resp.StatusCode, tt.statusCode)
			}
		})
	}
}

func Test_RoutePermission(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		method     string
		subject    string
		statusCode int
	}{
		{
			name:       "alice has permission to create blog",
			url:        "/blog",
			method:     "POST",
			subject:    "alice",
			statusCode: 200,
		},
		{
			name:       "alice has permission to update blog",
			url:        "/blog/1",
			method:     "PUT",
			subject:    "alice",
			statusCode: 200,
		},
		{
			name:       "bob has only permission to create comment",
			url:        "/comment",
			method:     "POST",
			subject:    "bob",
			statusCode: 200,
		},
		{
			name:       "unauthenticated user has no permissions",
			url:        "/",
			method:     "POST",
			subject:    "",
			statusCode: 401,
		},
		{
			name:       "bob has not permission to create blog",
			url:        "/blog",
			method:     "POST",
			subject:    "bob",
			statusCode: 403,
		},
		{
			name:       "bob has not permission to delete blog",
			url:        "/blog/1",
			method:     "DELETE",
			subject:    "bob",
			statusCode: 403,
		},
	}

	app := *fiber.New()
	authz := New(Config{
		ModelFilePath: "./example/model.conf",
		PolicyAdapter: fileadapter.NewAdapter("./example/policy.csv"),
		Lookup: func(c *fiber.Ctx) string {
			return c.Get("x-subject")
		},
	})

	app.Use(authz.RoutePermission())
	app.Post("/blog",
		func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Put("/blog/:id",
		func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Delete("/blog/:id",
		func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Post("/comment",
		func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)

	for _, tt := range tests {

		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, tt.url, nil)
			req.Header.Set("x-subject", tt.subject)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf(`%s: %s`, t.Name(), err)
			}

			if resp.StatusCode != tt.statusCode {
				t.Fatalf(`%s: StatusCode: got %v - expected %v`, t.Name(), resp.StatusCode, tt.statusCode)
			}
		})
	}
}

func Test_ModeEnforcer(t *testing.T) {
	tests := []struct {
		name       string
		url        string
		method     string
		subject    string
		statusCode int
	}{
		{
			name:       "alice has permission to create blog",
			url:        "/blog",
			method:     "POST",
			subject:    "alice",
			statusCode: 200,
		},
		{
			name:       "alice has permission to update blog",
			url:        "/blog/1",
			method:     "PUT",
			subject:    "alice",
			statusCode: 200,
		},
		{
			name:       "bob has only permission to create comment",
			url:        "/comment",
			method:     "POST",
			subject:    "bob",
			statusCode: 200,
		},
		{
			name:       "unauthenticated user has no permissions",
			url:        "/",
			method:     "POST",
			subject:    "",
			statusCode: 401,
		},
		{
			name:       "bob has not permission to create blog",
			url:        "/blog",
			method:     "POST",
			subject:    "bob",
			statusCode: 403,
		},
		{
			name:       "bob has not permission to delete blog",
			url:        "/blog/1",
			method:     "DELETE",
			subject:    "bob",
			statusCode: 403,
		},
	}

	app := *fiber.New()

	enforcer, err := casbin.NewEnforcer("./example/model.conf", fileadapter.NewAdapter("./example/policy.csv"))
	if err != nil {
		log.Fatal(err)
	}

	authz := New(Config{
		Enforcer: enforcer,
		Lookup: func(c *fiber.Ctx) string {
			return c.Get("x-subject")
		},
	})

	app.Use(authz.RoutePermission())
	app.Post("/blog",
		func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Put("/blog/:id",
		func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Delete("/blog/:id",
		func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)
	app.Post("/comment",
		func(c *fiber.Ctx) error {
			return c.SendStatus(fiber.StatusOK)
		},
	)

	for _, tt := range tests {

		t.Run(tt.name, func(t *testing.T) {
			req, _ := http.NewRequest(tt.method, tt.url, nil)
			req.Header.Set("x-subject", tt.subject)
			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf(`%s: %s`, t.Name(), err)
			}

			if resp.StatusCode != tt.statusCode {
				t.Fatalf(`%s: StatusCode: got %v - expected %v`, t.Name(), resp.StatusCode, tt.statusCode)
			}
		})
	}
}
