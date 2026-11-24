package opa

import (
	"context"
	"fmt"
	"io"
	"maps"
	"strings"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/gofiber/utils/v2"
	utilsstrings "github.com/gofiber/utils/v2/strings"
	"github.com/open-policy-agent/opa/v1/rego"
)

const evalErrorMessage = "Error evaluating rego policy"

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
		// A nil map leaves the input null unless query or headers have to go into it.
		if input == nil && (cfg.IncludeQueryString || len(cfg.IncludeHeaders) > 0) {
			input = make(map[string]any)
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
				// Readers pick different lines of a repeated field, so it is refused; Cookie lines form one list.
				if lines := c.GetAll(header); len(lines) > 1 || headerLines(c, header) > 1 {
					if !utils.EqualFold(header, fiber.HeaderCookie) {
						c.Response().SetStatusCode(cfg.DeniedStatusCode)
						c.Response().SetBodyString(cfg.DeniedResponseMessage)
						return nil
					}
					headers[header] = strings.Join(lines, "; ")
					continue
				}
				headers[header] = c.Get(header)
			}
			input["headers"] = headers
		}
		res, err := query.Eval(context.Background(), rego.EvalInput(input))
		// The router serves other readings of the request alike, so the policy has to allow those too.
		if err == nil && res.Allowed() {
			for _, in := range routerInputs(c, input) {
				if res, err = query.Eval(context.Background(), rego.EvalInput(in)); err != nil || !res.Allowed() {
					break
				}
			}
		}
		if err != nil {
			// OPA errors can quote values the policy computed, so they go to the log, not the client.
			log.Errorf("opa: error evaluating rego policy: %v", err)
			c.Response().SetStatusCode(fiber.StatusInternalServerError)
			c.Response().SetBodyString(evalErrorMessage)
			return fiber.NewError(fiber.StatusInternalServerError, evalErrorMessage)
		}

		if !res.Allowed() {
			c.Response().SetStatusCode(cfg.DeniedStatusCode)
			c.Response().SetBodyString(cfg.DeniedResponseMessage)
			return nil
		}

		return c.Next()
	}
}

// routerInputs returns copies of input with each other path spelling, if input carries c.Path(),
// and with GET for HEAD, which the router answers with GET handlers unless DisableHeadAutoRegister.
func routerInputs(c fiber.Ctx, input map[string]any) []map[string]any {
	var inputs []map[string]any
	if input["path"] == c.Path() {
		for _, path := range routerPaths(c) {
			in := maps.Clone(input)
			in["path"] = path
			inputs = append(inputs, in)
		}
	}
	if method := c.Method(); method == fiber.MethodHead && input["method"] == method && !c.App().Config().DisableHeadAutoRegister {
		for _, in := range append([]map[string]any{input}, inputs...) {
			in = maps.Clone(in)
			in["method"] = fiber.MethodGet
			inputs = append(inputs, in)
		}
	}
	return inputs
}

// routerPaths returns the other spellings of c.Path() the router works with: slash-trimmed,
// which route params are cut from, and that case-folded, which routes match on.
func routerPaths(c fiber.Ctx) []string {
	path := c.Path()
	cfg := c.App().Config()
	var paths []string

	trimmed := path
	if !cfg.StrictRouting && len(trimmed) > 1 {
		trimmed = utils.TrimRight(trimmed, '/')
		// Slashes alone are the root, as in the router.
		if trimmed == "" {
			trimmed = "/"
		}
	}
	if trimmed != path {
		paths = append(paths, trimmed)
	}

	if !cfg.CaseSensitive {
		if routed := utilsstrings.ToLower(trimmed); routed != trimmed {
			paths = append(paths, routed)
		}
	}
	return paths
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

// headerLines counts the lines of a header in any letter case, empty ones included.
func headerLines(c fiber.Ctx, name string) int {
	n := 0
	for key := range c.Request().Header.All() {
		if utils.EqualFold(utils.UnsafeString(key), name) {
			n++
		}
	}
	return n
}
