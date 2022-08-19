package opafiber

import (
	"context"
	"fmt"
	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/open-policy-agent/opa/rego"
	"io"
)

type Config struct {
	RegoPolicy            io.Reader
	RegoQuery             string
	IncludeHeaders        []string
	IncludeQueryString    bool
	DeniedStatusCode      int
	DeniedResponseMessage string
}

func New(cfg Config) fiber.Handler {
	if cfg.RegoQuery == "" {
		panic(fmt.Sprint("rego query can not be empty"))
	}

	if cfg.DeniedStatusCode == 0 {
		cfg.DeniedStatusCode = fiber.StatusBadRequest
	}

	readedBytes, err := io.ReadAll(cfg.RegoPolicy)
	if err != nil {
		panic(fmt.Sprint("could not read rego policy %w", err))
	}

	query, err := rego.New(
		rego.Query(cfg.RegoQuery),
		rego.Module("policy.rego", utils.UnsafeString(readedBytes)),
	).PrepareForEval(context.Background())

	if err != nil {
		panic(fmt.Sprint("rego policy error: %w", err))
	}

	return func(c *fiber.Ctx) error {
		input := map[string]interface{}{
			"method": c.Method(),
			"path":   c.Path(),
		}

		if cfg.IncludeQueryString {
			queryStringData := make(map[string][]string)
			c.Request().URI().QueryArgs().VisitAll(func(key, value []byte) {
				queryStringData[utils.UnsafeString(key)] = append(queryStringData[utils.UnsafeString(key)], utils.UnsafeString(value))
			})
			input["query"] = queryStringData
		}

		if len(cfg.IncludeHeaders) > 0 {
			headers := make(map[string]string)
			for _, header := range cfg.IncludeHeaders {
				headers[header] = c.Get(header)
			}
			input["headers"] = headers
		}

		resultSet, err := query.Eval(context.Background(), rego.EvalInput(input))
		if err != nil {
			panic(fmt.Sprint("rego evaluation error: %w", err))
		}

		if !resultSet.Allowed() {
			c.Response().SetStatusCode(cfg.DeniedStatusCode)
			c.Response().SetBodyString(cfg.DeniedResponseMessage)
			return nil
		}

		return c.Next()
	}
}
