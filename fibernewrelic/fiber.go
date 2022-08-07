package fibernewrelic

import (
	"fmt"
	"github.com/gofiber/fiber/v2"
	"github.com/newrelic/go-agent/v3/newrelic"
	"net/url"
)

type Config struct {
	// License parameter is required to initialize newrelic application
	License string
	// AppName parameter is required to initialize newrelic application, default is fiber-api
	AppName string
	// Enabled parameter passed to enable/disable newrelic
	Enabled bool
	// TransportType can be HTTP or HTTPS (case-sensitive), default is HTTP
	TransportType string
}

var ConfigDefault = Config{
	License:       "",
	AppName:       "fiber-api",
	Enabled:       false,
	TransportType: string(newrelic.TransportHTTP),
}

func New(cfg Config) fiber.Handler {
	if cfg.TransportType != "HTTP" && cfg.TransportType != "HTTPS" {
		cfg.TransportType = ConfigDefault.TransportType
	}

	if cfg.AppName == "" {
		cfg.AppName = ConfigDefault.AppName
	}

	if cfg.License == "" {
		panic(fmt.Errorf("unable to create New Relic Application -> License can not be empty"))
	}

	app, err := newrelic.NewApplication(
		newrelic.ConfigAppName(cfg.AppName),
		newrelic.ConfigLicense(cfg.License),
		newrelic.ConfigEnabled(cfg.Enabled),
	)

	if err != nil {
		panic(fmt.Errorf("unable to create New Relic Application -> %w", err))
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
