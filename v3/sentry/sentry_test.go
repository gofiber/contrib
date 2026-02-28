package sentry

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
)

type testCase struct {
	desc    string
	path    string
	method  string
	body    string
	handler fiber.Handler
	event   *sentry.Event
}

func testCasesBeforeRegister(t *testing.T) []testCase {
	return []testCase{
		{
			desc:   "MustGetHubFromContext without Sentry middleware",
			path:   "/no-middleware",
			method: "GET",
			handler: func(c fiber.Ctx) error {
				defer func() {
					if r := recover(); r == nil {
						t.Fatal("MustGetHubFromContext did not panic")
					}
				}()
				_ = MustGetHubFromContext(c) // This should panic
				return nil
			},
			event: nil, // No event expected because a panic should occur
		},
		{
			desc:   "GetHubFromContext without Sentry middleware",
			path:   "/no-middleware-2",
			method: "GET",
			handler: func(c fiber.Ctx) error {
				hub := GetHubFromContext(c)
				if hub != nil {
					t.Fatal("Expected nil, got a Sentry hub instance")
				}
				return nil
			},
			event: nil, // No Sentry event expected here
		},
	}
}

var testCasesAfterRegister = []testCase{
	{
		desc:   "panic",
		path:   "/panic",
		method: "GET",
		handler: func(c fiber.Ctx) error {
			panic("test")
		},
		event: &sentry.Event{
			Level:   sentry.LevelFatal,
			Message: "test",
			Request: &sentry.Request{
				URL:    "http://example.com/panic",
				Method: "GET",
				Headers: map[string]string{
					"Host":       "example.com",
					"User-Agent": "fiber",
				},
			},
		},
	},
	{
		desc:   "post",
		path:   "/post",
		method: "POST",
		body:   "payload",
		handler: func(c fiber.Ctx) error {
			hub := MustGetHubFromContext(c)
			hub.CaptureMessage("post: " + string(c.Body()))
			return nil
		},
		event: &sentry.Event{
			Level:   sentry.LevelInfo,
			Message: "post: payload",
			Request: &sentry.Request{
				URL:    "http://example.com/post",
				Method: "POST",
				Data:   "payload",
				Headers: map[string]string{
					"Content-Length": "7",
					"Host":           "example.com",
					"User-Agent":     "fiber",
				},
			},
		},
	},
	{
		desc:   "get",
		path:   "/get",
		method: "GET",
		handler: func(c fiber.Ctx) error {
			hub := MustGetHubFromContext(c)
			hub.CaptureMessage("get")
			return nil
		},
		event: &sentry.Event{
			Level:   sentry.LevelInfo,
			Message: "get",
			Request: &sentry.Request{
				URL:    "http://example.com/get",
				Method: "GET",
				Headers: map[string]string{
					"Host":       "example.com",
					"User-Agent": "fiber",
				},
			},
		},
	},
	{
		desc:   "large body",
		path:   "/post/large",
		method: "POST",
		body:   strings.Repeat("Large", 3*1024), // 15 KB
		handler: func(c fiber.Ctx) error {
			hub := MustGetHubFromContext(c)
			hub.CaptureMessage(fmt.Sprintf("post: %d KB", len(c.Body())/1024))
			return nil
		},
		event: &sentry.Event{
			Level:   sentry.LevelInfo,
			Message: "post: 15 KB",
			Request: &sentry.Request{
				URL:    "http://example.com/post/large",
				Method: "POST",
				// Actual request body omitted because too large.
				Data: "",
				Headers: map[string]string{
					"Content-Length": "15360",
					"Host":           "example.com",
					"User-Agent":     "fiber",
				},
			},
		},
	},
	{
		desc:   "ignore body",
		path:   "/post/body-ignored",
		method: "POST",
		body:   "client sends, fasthttp always reads, SDK reports",
		handler: func(c fiber.Ctx) error {
			hub := MustGetHubFromContext(c)
			hub.CaptureMessage("body ignored")
			return nil
		},
		event: &sentry.Event{
			Level:   sentry.LevelInfo,
			Message: "body ignored",
			Request: &sentry.Request{
				URL:    "http://example.com/post/body-ignored",
				Method: "POST",
				// Actual request body included because fasthttp always
				// reads full request body.
				Data: "client sends, fasthttp always reads, SDK reports",
				Headers: map[string]string{
					"Content-Length": "48",
					"Host":           "example.com",
					"User-Agent":     "fiber",
				},
			},
		},
	},
}

func Test_Sentry(t *testing.T) {
	app := fiber.New()

	testFunc := func(t *testing.T, tC testCase) {
		t.Run(tC.desc, func(t *testing.T) {
			if err := sentry.Init(sentry.ClientOptions{
				BeforeSend: func(event *sentry.Event, hint *sentry.EventHint) *sentry.Event {
					require.Equal(t, tC.event.Message, event.Message)
					require.Equal(t, tC.event.Request, event.Request)
					require.Equal(t, tC.event.Level, event.Level)
					require.Equal(t, tC.event.Exception, event.Exception)
					return event
				},
			}); err != nil {
				t.Fatal(err)
			}

			app.Add([]string{tC.method}, tC.path, tC.handler)

			req, err := http.NewRequest(tC.method, "http://example.com"+tC.path, strings.NewReader(tC.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("User-Agent", "fiber")

			resp, err := app.Test(req)
			if err != nil {
				t.Fatalf("Request %q failed: %s", tC.path, err)
			}
			resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("Status code = %d", resp.StatusCode)
			}
		})
	}

	for _, tC := range testCasesBeforeRegister(t) {
		testFunc(t, tC)
	}

	app.Use(New())

	for _, tC := range testCasesAfterRegister {
		testFunc(t, tC)
	}

	if ok := sentry.Flush(time.Second); !ok {
		t.Fatal("sentry.Flush timed out")
	}
}

func Test_GetHubFromContext_PassLocalsToContext(t *testing.T) {
	app := fiber.New(fiber.Config{PassLocalsToContext: true})
	app.Use(New())

	app.Get("/", func(c fiber.Ctx) error {
		hub := GetHubFromContext(c)
		hubFromContext := GetHubFromAnyContext(c.Context())
		require.NotNil(t, hub)
		require.NotNil(t, hubFromContext)
		return c.SendStatus(http.StatusOK)
	})

	req, err := http.NewRequest(http.MethodGet, "http://example.com/", nil)
	require.NoError(t, err)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
}
