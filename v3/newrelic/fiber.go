package newrelic

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/gofiber/utils/v2"

	"github.com/gofiber/fiber/v3"
	"github.com/newrelic/go-agent/v3/newrelic"
)

type Config struct {
	// License parameter is required to initialize newrelic application
	License string
	// AppName parameter passed to set app name, default is fiber-api
	AppName string
	// Enabled parameter passed to enable/disable newrelic
	Enabled bool
	// TransportType can be HTTP or HTTPS, default is HTTP
	// Deprecated: The Transport type now acquiring from request URL scheme internally
	TransportType string
	// Application field is required to use an existing newrelic application
	Application *newrelic.Application
	// ErrorStatusCodeHandler is executed when an error is returned from handler
	// Optional. Default: DefaultErrorStatusCodeHandler
	ErrorStatusCodeHandler func(c fiber.Ctx, err error) int
	// Next defines a function to skip this middleware when returned true.
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool
	// RequestHeaderFilter controls which inbound request headers are forwarded to
	// New Relic via WebRequest.Header.
	// Return true to include a header, false to exclude it.
	// Optional. Default: include all headers.
	RequestHeaderFilter func(key, value string) bool
}

var ConfigDefault = Config{
	Application:            nil,
	License:                "",
	AppName:                "fiber-api",
	Enabled:                false,
	ErrorStatusCodeHandler: DefaultErrorStatusCodeHandler,
	Next:                   nil,
	RequestHeaderFilter:    nil,
}

func New(cfg Config) fiber.Handler {
	var app *newrelic.Application
	var err error

	if cfg.ErrorStatusCodeHandler == nil {
		cfg.ErrorStatusCodeHandler = ConfigDefault.ErrorStatusCodeHandler
	}

	if cfg.Application != nil {
		app = cfg.Application
	} else {
		if cfg.AppName == "" {
			cfg.AppName = ConfigDefault.AppName
		}

		if cfg.License == "" {
			panic(fmt.Errorf("unable to create New Relic Application -> License can not be empty"))
		}

		app, err = newrelic.NewApplication(
			newrelic.ConfigAppName(cfg.AppName),
			newrelic.ConfigLicense(cfg.License),
			newrelic.ConfigEnabled(cfg.Enabled),
		)

		if err != nil {
			panic(fmt.Errorf("unable to create New Relic Application -> %w", err))
		}
	}

	return func(c fiber.Ctx) error {
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		txn := app.StartTransaction(createTransactionName(c))
		defer txn.End()

		var (
			host   = utils.CopyString(c.Hostname())
			method = utils.CopyString(c.Method())
		)

		scheme := c.Request().URI().Scheme()
		txn.SetWebRequest(createWebRequest(c, host, method, string(scheme), cfg.RequestHeaderFilter))

		c.SetContext(newrelic.NewContext(c.Context(), txn))

		handlerErr := c.Next()
		statusCode := c.RequestCtx().Response.StatusCode()

		if handlerErr != nil {
			statusCode = cfg.ErrorStatusCodeHandler(c, handlerErr)
			txn.NoticeError(handlerErr)
		}

		txn.SetWebResponse(nil).WriteHeader(statusCode)

		return handlerErr
	}
}

// FromContext returns the Transaction from the context if present, and nil
// otherwise.
func FromContext(c fiber.Ctx) *newrelic.Transaction {
	return newrelic.FromContext(c.Context())
}

func createTransactionName(c fiber.Ctx) string {
	return fmt.Sprintf("%s %s", c.Request().Header.Method(), c.Request().URI().Path())
}

func createWebRequest(c fiber.Ctx, host, method, scheme string, filter func(key, value string) bool) newrelic.WebRequest {
	headers := make(http.Header, c.Request().Header.Len())
	c.Request().Header.VisitAll(func(key, value []byte) {
		headerKey := string(key)
		headerValue := string(value)

		if filter != nil && !filter(headerKey, headerValue) {
			return
		}

		headers.Add(headerKey, headerValue)
	})

	return newrelic.WebRequest{
		Header:    headers,
		Host:      host,
		Method:    method,
		Transport: transport(scheme),
		URL: &url.URL{
			Host:     host,
			Scheme:   scheme,
			Path:     string(c.Request().URI().Path()),
			RawQuery: string(c.Request().URI().QueryString()),
		},
	}
}

func transport(schema string) newrelic.TransportType {
	if strings.HasPrefix(schema, "https") {
		return newrelic.TransportHTTPS
	}

	if strings.HasPrefix(schema, "http") {
		return newrelic.TransportHTTP
	}

	return newrelic.TransportUnknown
}

func DefaultErrorStatusCodeHandler(c fiber.Ctx, err error) int {
	if fiberErr, ok := err.(*fiber.Error); ok {
		return fiberErr.Code
	}

	return c.RequestCtx().Response.StatusCode()
}
