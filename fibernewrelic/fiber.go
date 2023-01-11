package fibernewrelic

import (
	"fmt"
	"github.com/gofiber/fiber/v2"
	"github.com/newrelic/go-agent/v3/newrelic"
	"net/url"
	"strings"
)

type Config struct {
	// License parameter is required to initialize newrelic application
	License string
	// AppName parameter passed to set app name, default is fiber-api
	AppName string
	// Enabled parameter passed to enable/disable newrelic
	Enabled bool
	// TransportType can be HTTP or HTTPS, default is HTTP
	TransportType string
	// Application field is required to use an existing newrelic application
	Application *newrelic.Application
}

var ConfigDefault = Config{
	Application:   nil,
	License:       "",
	AppName:       "fiber-api",
	Enabled:       false,
	TransportType: string(newrelic.TransportHTTP),
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

	normalizeTransport := strings.ToUpper(cfg.TransportType)

	if normalizeTransport != "HTTP" && normalizeTransport != "HTTPS" {
		cfg.TransportType = ConfigDefault.TransportType
	} else {
		cfg.TransportType = normalizeTransport
	}

	return func(c *fiber.Ctx) error {
		txn := app.StartTransaction(c.Method() + " " + c.Path())
		originalURL, err := url.Parse(c.OriginalURL())
		if err != nil {
			return c.Next()
		}

		txn.SetWebRequest(newrelic.WebRequest{
			URL:       originalURL,
			Method:    c.Method(),
			Transport: newrelic.TransportType(cfg.TransportType),
			Host:      c.Hostname(),
		})

		err = c.Next()
		if err != nil {
			txn.NoticeError(err)
		}

		defer txn.SetWebResponse(nil).WriteHeader(c.Response().StatusCode())
		defer txn.End()

		return err
	}
}
