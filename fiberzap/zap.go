package fiberzap

import (
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gofiber/fiber/v2"
	"go.uber.org/zap"
)

// New creates a new middleware handler
func New(config ...Config) fiber.Handler {
	// Set default config
	cfg := configDefault(config...)

	// Set PID once
	pid := strconv.Itoa(os.Getpid())

	// Set variables
	var (
		once       sync.Once
		errHandler fiber.ErrorHandler
	)

	var errPadding = 15
	var latencyEnabled = contains("latency", cfg.Fields)

	// put ignore uri into a map for faster match
	skipURIs := make(map[string]struct{})
	for _, uri := range cfg.SkipURIs {
		skipURIs[uri] = struct{}{}
	}

	// Return new handler
	return func(c *fiber.Ctx) (err error) {
		// Don't execute middleware if Next returns true
		if cfg.Next != nil && cfg.Next(c) {
			return c.Next()
		}

		// skip uri
		if _, ok := skipURIs[c.Path()]; ok {
			return c.Next()
		}

		// Set error handler once
		once.Do(func() {
			// get longested possible path
			stack := c.App().Stack()
			for m := range stack {
				for r := range stack[m] {
					if len(stack[m][r].Path) > errPadding {
						errPadding = len(stack[m][r].Path)
					}
				}
			}
			// override error handler
			errHandler = c.App().Config().ErrorHandler
		})

		var start, stop time.Time

		if latencyEnabled {
			start = time.Now()
		}

		// Handle request, store err for logging
		chainErr := c.Next()

		// Manually call error handler
		if chainErr != nil {
			if err := errHandler(c, chainErr); err != nil {
				_ = c.SendStatus(fiber.StatusInternalServerError)
			}
		}

		// Set latency stop time
		if latencyEnabled {
			stop = time.Now()
		}

		// Check if the logger has the appropriate level
		var (
			s     = c.Response().StatusCode()
			index int
		)
		switch {
		case s >= 500:
			// error index is zero
		case s >= 400:
			index = 1
		default:
			index = 2
		}
		levelIndex := index
		if levelIndex >= len(cfg.Levels) {
			levelIndex = len(cfg.Levels) - 1
		}
		messageIndex := index
		if messageIndex >= len(cfg.Messages) {
			messageIndex = len(cfg.Messages) - 1
		}

		ce := cfg.Logger.Check(cfg.Levels[levelIndex], cfg.Messages[messageIndex])
		if ce == nil {
			return nil
		}

		// Add fields
		fields := make([]zap.Field, 0, len(cfg.Fields)+1)
		fields = append(fields, zap.Error(err))

		for _, field := range cfg.Fields {
			switch field {
			case "referer":
				fields = append(fields, zap.String("referer", c.Get(fiber.HeaderReferer)))
			case "protocol":
				fields = append(fields, zap.String("protocol", c.Protocol()))
			case "pid":
				fields = append(fields, zap.String("pid", pid))
			case "port":
				fields = append(fields, zap.String("port", c.Port()))
			case "ip":
				fields = append(fields, zap.String("ip", c.IP()))
			case "ips":
				fields = append(fields, zap.String("ips", c.Get(fiber.HeaderXForwardedFor)))
			case "host":
				fields = append(fields, zap.String("host", c.Hostname()))
			case "path":
				fields = append(fields, zap.String("path", c.Path()))
			case "url":
				fields = append(fields, zap.String("url", c.OriginalURL()))
			case "ua":
				fields = append(fields, zap.String("ua", c.Get(fiber.HeaderUserAgent)))
			case "latency":
				fields = append(fields, zap.String("latency", stop.Sub(start).String()))
			case "status":
				fields = append(fields, zap.Int("status", c.Response().StatusCode()))
			case "resBody":
				if cfg.SkipResBody == nil || !cfg.SkipResBody(c) {
					if cfg.GetResBody == nil {
						fields = append(fields, zap.ByteString("resBody", c.Response().Body()))
					} else {
						fields = append(fields, zap.ByteString("resBody", cfg.GetResBody(c)))
					}
				}
			case "queryParams":
				fields = append(fields, zap.String("queryParams", c.Request().URI().QueryArgs().String()))
			case "body":
				if cfg.SkipBody == nil || !cfg.SkipBody(c) {
					fields = append(fields, zap.ByteString("body", c.Body()))
				}
			case "bytesReceived":
				fields = append(fields, zap.Int("bytesReceived", len(c.Request().Body())))
			case "bytesSent":
				fields = append(fields, zap.Int("bytesSent", len(c.Response().Body())))
			case "route":
				fields = append(fields, zap.String("route", c.Route().Path))
			case "method":
				fields = append(fields, zap.String("method", c.Method()))
			case "requestId":
				fields = append(fields, zap.String("requestId", c.GetRespHeader(fiber.HeaderXRequestID)))
			case "error":
				if chainErr != nil {
					fields = append(fields, zap.String("error", chainErr.Error()))
				}
			case "reqHeaders":
				c.Request().Header.VisitAll(func(k, v []byte) {
					fields = append(fields, zap.ByteString(string(k), v))
				})
			}
		}

		ce.Write(fields...)

		return nil
	}
}

func contains(needle string, slice []string) bool {
	for _, e := range slice {
		if e == needle {
			return true
		}
	}

	return false
}
