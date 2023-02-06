package fibernewrelic

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
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
}

var ConfigDefault = Config{
	Application: nil,
	License:     "",
	AppName:     "fiber-api",
	Enabled:     false,
}

func New(cfg Config) fiber.Handler {
	var app *newrelic.Application
	var err error

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

	return func(c *fiber.Ctx) error {
		txn := app.StartTransaction("")
		defer txn.End()

		err := c.Next()

		method := utils.CopyString(c.Method())
		routePath := utils.CopyString(c.Route().Path)
		host := string(c.Request().URI().Host())

		u := url.URL{
			Scheme:   string(c.Request().URI().Scheme()),
			Host:     host,
			Path:     string(c.Request().URI().Path()),
			RawQuery: string(c.Request().URI().QueryString()),
		}

		txn.SetWebRequest(newrelic.WebRequest{
			URL:       &u,
			Method:    method,
			Transport: transport(u.Scheme),
			Host:      host,
		})
		txn.SetName(fmt.Sprintf("%s %s", method, routePath))

		statusCode := c.Context().Response.StatusCode()

		if err != nil {
			if fiberErr, ok := err.(*fiber.Error); ok {
				statusCode = fiberErr.Code
			}

			txn.NoticeError(err)
		}

		txn.SetWebResponse(nil).WriteHeader(statusCode)

		return err
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
