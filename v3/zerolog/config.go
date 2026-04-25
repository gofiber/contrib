package zerolog

import (
	"os"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/rs/zerolog"
)

const (
	FieldReferer       = "referer"
	FieldProtocol      = "protocol"
	FieldPID           = "pid"
	FieldPort          = "port"
	FieldIP            = "ip"
	FieldIPs           = "ips"
	FieldHost          = "host"
	FieldPath          = "path"
	FieldURL           = "url"
	FieldUserAgent     = "ua"
	FieldLatency       = "latency"
	FieldStatus        = "status"
	FieldResBody       = "resBody"
	FieldQueryParams   = "queryParams"
	FieldBody          = "body"
	FieldBytesReceived = "bytesReceived"
	FieldBytesSent     = "bytesSent"
	FieldRoute         = "route"
	FieldMethod        = "method"
	FieldRequestID     = "requestId"
	FieldError         = "error"
	FieldReqHeaders    = "reqHeaders"
	FieldResHeaders    = "resHeaders"

	fieldResBody_       = "res_body"
	fieldQueryParams_   = "query_params"
	fieldBytesReceived_ = "bytes_received"
	fieldBytesSent_     = "bytes_sent"
	fieldRequestID_     = "request_id"
	fieldReqHeaders_    = "req_headers"
	fieldResHeaders_    = "res_headers"
)

// Config defines the config for middleware.
type Config struct {
	// Next defines a function to skip this middleware when returned true.
	//
	// Optional. Default: nil
	Next func(c fiber.Ctx) bool

	// SkipField defines a function that returns true if a specific field should be skipped from logging.
	//
	// Optional. Default: nil
	SkipField func(field string, c fiber.Ctx) bool

	// GetResBody defines a function to get a custom response body.
	// e.g. when using compress middleware, the original response body may be unreadable.
	// You can use GetResBody to provide a readable body.
	//
	// Optional. Default: nil
	GetResBody func(c fiber.Ctx) []byte

	// Add a custom zerolog logger.
	//
	// Optional. Default: zerolog.New(os.Stderr).With().Timestamp().Logger()
	Logger *zerolog.Logger

	// GetLogger defines a function to get a custom zerolog logger.
	// e.g. when creating a new logger for each request.
	//
	// GetLogger will override Logger.
	//
	// Optional. Default: nil
	GetLogger func(c fiber.Ctx) zerolog.Logger

	// Add the fields you want to log.
	//
	// Optional. Default: {"ip", "latency", "status", "method", "url", "error"}
	Fields []string

	// Defines a function that returns true if a header should not be logged.
	// Only relevant if `FieldReqHeaders` and/or `FieldResHeaders` are logged.
	//
	// Optional. Default: nil
	SkipHeader func(header string, c fiber.Ctx) bool

	// Wrap headers into a dictionary.
	// If false: {"method":"POST", "header-key":"header value"}
	// If true: {"method":"POST", "reqHeaders": {"header-key":"header value"}}
	//
	// Optional. Default: false
	WrapHeaders bool

	// Use snake case for fields: FieldResBody, FieldQueryParams, FieldBytesReceived, FieldBytesSent, FieldRequestID, FieldReqHeaders, FieldResHeaders.
	// If false: {"method":"POST", "resBody":"v", "queryParams":"v"}
	// If true: {"method":"POST", "res_body":"v", "query_params":"v"}
	//
	// Optional. Default: false
	FieldsSnakeCase bool

	// Custom response messages.
	// Response codes >= 500 will be logged with Messages[0].
	// Response codes >= 400 will be logged with Messages[1].
	// Other response codes will be logged with Messages[2].
	// You can specify fewer than 3 messages, but you must specify at least 1.
	// Specifying more than 3 messages is useless.
	//
	// Optional. Default: {"Server error", "Client error", "Success"}
	Messages []string

	// Custom response levels.
	// Response codes >= 500 will be logged with Levels[0].
	// Response codes >= 400 will be logged with Levels[1].
	// Other response codes will be logged with Levels[2].
	// You can specify fewer than 3 levels, but you must specify at least 1.
	// Specifying more than 3 levels is useless.
	//
	// Optional. Default: {zerolog.ErrorLevel, zerolog.WarnLevel, zerolog.InfoLevel}
	Levels []zerolog.Level
}

func (c *Config) loggerCtx(fc fiber.Ctx) zerolog.Context {
	if c.GetLogger != nil {
		return c.GetLogger(fc).With()
	}

	return c.Logger.With()
}

func (c *Config) logger(fc fiber.Ctx, latency time.Duration, err error) zerolog.Logger {
	zc := c.loggerCtx(fc)

	for _, field := range c.Fields {
		if c.SkipField != nil && c.SkipField(field, fc) {
			continue
		}
		switch field {
		case FieldReferer:
			zc = zc.Str(field, fc.Get(fiber.HeaderReferer))
		case FieldProtocol:
			zc = zc.Str(field, fc.Protocol())
		case FieldPID:
			zc = zc.Int(field, os.Getpid())
		case FieldPort:
			zc = zc.Str(field, fc.Port())
		case FieldIP:
			zc = zc.Str(field, fc.IP())
		case FieldIPs:
			zc = zc.Str(field, fc.Get(fiber.HeaderXForwardedFor))
		case FieldHost:
			zc = zc.Str(field, fc.Hostname())
		case FieldPath:
			zc = zc.Str(field, fc.Path())
		case FieldURL:
			zc = zc.Str(field, fc.OriginalURL())
		case FieldUserAgent:
			zc = zc.Str(field, fc.Get(fiber.HeaderUserAgent))
		case FieldLatency:
			zc = zc.Str(field, latency.String())
		case FieldStatus:
			zc = zc.Int(field, fc.Response().StatusCode())
		case FieldBody:
			zc = zc.Str(field, string(fc.Body()))
		case FieldResBody:
			if c.FieldsSnakeCase {
				field = fieldResBody_
			}
			resBody := fc.Response().Body()
			if c.GetResBody != nil {
				if customResBody := c.GetResBody(fc); customResBody != nil {
					resBody = customResBody
				}
			}
			zc = zc.Str(field, string(resBody))
		case FieldQueryParams:
			if c.FieldsSnakeCase {
				field = fieldQueryParams_
			}
			zc = zc.Stringer(field, fc.Request().URI().QueryArgs())
		case FieldBytesReceived:
			if c.FieldsSnakeCase {
				field = fieldBytesReceived_
			}
			zc = zc.Int(field, len(fc.Request().Body()))
		case FieldBytesSent:
			if c.FieldsSnakeCase {
				field = fieldBytesSent_
			}
			zc = zc.Int(field, len(fc.Response().Body()))
		case FieldRoute:
			zc = zc.Str(field, fc.Route().Path)
		case FieldMethod:
			zc = zc.Str(field, fc.Method())
		case FieldRequestID:
			if c.FieldsSnakeCase {
				field = fieldRequestID_
			}
			zc = zc.Str(field, fc.GetRespHeader(fiber.HeaderXRequestID))
		case FieldError:
			if err != nil {
				zc = zc.Err(err)
			}
		case FieldReqHeaders:
			if c.FieldsSnakeCase {
				field = fieldReqHeaders_
			}
			if c.WrapHeaders {
				dict := zerolog.Dict()
				for header, values := range fc.GetReqHeaders() {
					if len(values) == 0 {
						continue
					}

					if c.SkipHeader != nil && c.SkipHeader(header, fc) {
						continue
					}

					if len(values) == 1 {
						dict.Str(header, values[0])
						continue
					}

					dict.Strs(header, values)
				}
				zc = zc.Dict(field, dict)
			} else {
				for header, values := range fc.GetReqHeaders() {
					if len(values) == 0 {
						continue
					}

					if c.SkipHeader != nil && c.SkipHeader(header, fc) {
						continue
					}

					if len(values) == 1 {
						zc = zc.Str(header, values[0])
						continue
					}

					zc = zc.Strs(header, values)
				}
			}
		case FieldResHeaders:
			if c.FieldsSnakeCase {
				field = fieldResHeaders_
			}
			if c.WrapHeaders {
				dict := zerolog.Dict()
				for header, values := range fc.GetRespHeaders() {
					if len(values) == 0 {
						continue
					}

					if c.SkipHeader != nil && c.SkipHeader(header, fc) {
						continue
					}

					if len(values) == 1 {
						dict.Str(header, values[0])
						continue
					}

					dict.Strs(header, values)
				}
				zc = zc.Dict(field, dict)
			} else {
				for header, values := range fc.GetRespHeaders() {
					if len(values) == 0 {
						continue
					}

					if c.SkipHeader != nil && c.SkipHeader(header, fc) {
						continue
					}

					if len(values) == 1 {
						zc = zc.Str(header, values[0])
						continue
					}

					zc = zc.Strs(header, values)
				}
			}
		}
	}

	return zc.Logger()
}

var logger = zerolog.New(os.Stderr).With().Timestamp().Logger()

// ConfigDefault is the default config
var ConfigDefault = Config{
	Next:     nil,
	Logger:   &logger,
	Fields:   []string{FieldIP, FieldLatency, FieldStatus, FieldMethod, FieldURL, FieldError},
	Messages: []string{"Server error", "Client error", "Success"},
	Levels:   []zerolog.Level{zerolog.ErrorLevel, zerolog.WarnLevel, zerolog.InfoLevel},
}

// Helper function to set default values
func configDefault(config ...Config) Config {
	// Return default config if nothing provided
	if len(config) < 1 {
		return ConfigDefault
	}

	// Override default config
	cfg := config[0]

	// Set default values
	if cfg.Next == nil {
		cfg.Next = ConfigDefault.Next
	}

	if cfg.Logger == nil {
		cfg.Logger = ConfigDefault.Logger
	}

	if cfg.Fields == nil {
		cfg.Fields = ConfigDefault.Fields
	}

	if cfg.Messages == nil {
		cfg.Messages = ConfigDefault.Messages
	}

	if cfg.Levels == nil {
		cfg.Levels = ConfigDefault.Levels
	}

	return cfg
}
