package opa

import (
	"context"
	"fmt"
	"io"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/utils/v2"
	utilsstrings "github.com/gofiber/utils/v2/strings"
	"github.com/open-policy-agent/opa/v1/rego"
)

type InputCreationFunc func(c fiber.Ctx) (map[string]any, error)

type Config struct {
	RegoPolicy            io.Reader
	RegoQuery             string
	IncludeHeaders        []string
	IncludeQueryString    bool
	DeniedStatusCode      int
	DeniedResponseMessage string
	InputCreationMethod   InputCreationFunc
}

func New(cfg Config) fiber.Handler {
	err := cfg.fillAndValidate()
	if err != nil {
		panic(err)
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
	return func(c fiber.Ctx) error {
		input, err := cfg.InputCreationMethod(c)
		if err != nil {
			c.Response().SetStatusCode(fiber.StatusInternalServerError)
			c.Response().SetBodyString(fmt.Sprintf("Error creating input: %s", err))
			return err
		}
		if cfg.IncludeQueryString {
			queryStringData := make(map[string][]string)
			for key, value := range c.Request().URI().QueryArgs().All() {
				k := utils.UnsafeString(key)
				queryStringData[k] = append(queryStringData[k], utils.UnsafeString(value))
			}
			input["query"] = queryStringData
		}
		if len(cfg.IncludeHeaders) > 0 {
			headers := make(map[string]string)
			for _, header := range cfg.IncludeHeaders {
				headers[header] = c.Get(header)
			}
			input["headers"] = headers
		}
		res, err := query.Eval(context.Background(), rego.EvalInput(input))
		// The router matches a case-folded, slash-trimmed path, so the policy has to allow that spelling too.
		if path := c.Path(); err == nil && res.Allowed() && input["path"] == path {
			if routed := routerPath(c); routed != path {
				input["path"] = routed
				res, err = query.Eval(context.Background(), rego.EvalInput(input))
			}
		}
		if err != nil {
			c.Response().SetStatusCode(fiber.StatusInternalServerError)
			c.Response().SetBodyString(fmt.Sprintf("Error evaluating rego policy: %s", err))
			return err
		}

		if !res.Allowed() {
			c.Response().SetStatusCode(cfg.DeniedStatusCode)
			c.Response().SetBodyString(cfg.DeniedResponseMessage)
			return nil
		}

		return c.Next()
	}
}

// routerPath returns c.Path() as the router matches it: ASCII lower-cased unless
// CaseSensitive, trailing slashes trimmed unless StrictRouting.
func routerPath(c fiber.Ctx) string {
	path := c.Path()
	cfg := c.App().Config()

	if !cfg.CaseSensitive {
		path = utilsstrings.ToLower(path)
	}
	if !cfg.StrictRouting && len(path) > 1 {
		path = utils.TrimRight(path, '/')
	}
	return path
}

func (c *Config) fillAndValidate() error {
	if c.RegoQuery == "" {
		return fmt.Errorf("rego query can not be empty")
	}

	if c.DeniedStatusCode == 0 {
		c.DeniedStatusCode = fiber.StatusBadRequest
	}
	if c.DeniedResponseMessage == "" {
		c.DeniedResponseMessage = fiber.ErrBadRequest.Error()
	}
	if c.IncludeHeaders == nil {
		c.IncludeHeaders = []string{}
	}
	if c.InputCreationMethod == nil {
		c.InputCreationMethod = defaultInput
	}
	return nil
}

func defaultInput(ctx fiber.Ctx) (map[string]any, error) {
	input := map[string]any{
		"method": ctx.Method(),
		"path":   ctx.Path(),
	}
	return input, nil
}
