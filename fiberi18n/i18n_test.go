package fiberi18n

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2/utils"
	"github.com/gofiber/fiber/v3"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

func newServer() *fiber.App {
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(ctx fiber.Ctx) error {
		return ctx.SendString(MustLocalize(ctx, "welcome"))
	})
	app.Get("/:name", func(ctx fiber.Ctx) error {
		return ctx.SendString(MustLocalize(ctx, &i18n.LocalizeConfig{
			MessageID: "welcomeWithName",
			TemplateData: map[string]string{
				"name": ctx.Params("name"),
			},
		}))
	})
	return app
}

var i18nApp = newServer()

func makeRequest(lang language.Tag, name string, app *fiber.App) (*http.Response, error) {
	path := "/" + name
	req, _ := http.NewRequestWithContext(context.Background(), "GET", path, nil)
	if lang != language.Und {
		req.Header.Add([]string{"Accept-Language"}, lang.String())
	}
	req.Method = "GET"
	req.RequestURI = path
	resp, err := app.Test(req)
	return resp, err
}

func TestI18nEN(t *testing.T) {
	t.Parallel()
	type args struct {
		lang language.Tag
		name string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "hello world",
			args: args{
				name: "",
				lang: language.English,
			},
			want: "hello",
		},
		{
			name: "hello alex",
			args: args{
				name: "alex",
				lang: language.English,
			},
			want: "hello alex",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := makeRequest(tt.args.lang, tt.args.name, i18nApp)
			utils.AssertEqual(t, err, nil)
			body, err := io.ReadAll(got.Body)
			utils.AssertEqual(t, err, nil)
			utils.AssertEqual(t, tt.want, string(body))
		})
	}
}

func TestI18nZH(t *testing.T) {
	type args struct {
		lang language.Tag
		name string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "hello world",
			args: args{
				name: "",
				lang: language.Chinese,
			},
			want: "你好",
		},
		{
			name: "hello alex",
			args: args{
				name: "alex",
				lang: language.Chinese,
			},
			want: "你好 alex",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := makeRequest(tt.args.lang, tt.args.name, i18nApp)
			utils.AssertEqual(t, err, nil)
			body, err := io.ReadAll(got.Body)
			utils.AssertEqual(t, err, nil)
			utils.AssertEqual(t, tt.want, string(body))
		})
	}
}

func TestParallelI18n(t *testing.T) {
	type args struct {
		lang language.Tag
		name string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{
			name: "hello world",
			args: args{
				name: "",
				lang: language.Chinese,
			},
			want: "你好",
		},
		{
			name: "hello alex",
			args: args{
				name: "alex",
				lang: language.Chinese,
			},
			want: "你好 alex",
		},
		{
			name: "hello peter",
			args: args{
				name: "peter",
				lang: language.English,
			},
			want: "hello peter",
		},
	}
	t.Parallel()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := makeRequest(tt.args.lang, tt.args.name, i18nApp)
			utils.AssertEqual(t, err, nil)
			body, err := io.ReadAll(got.Body)
			utils.AssertEqual(t, err, nil)
			utils.AssertEqual(t, tt.want, string(body))
		})
	}
}

func TestLocalize(t *testing.T) {
	t.Parallel()
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(ctx fiber.Ctx) error {
		localize, err := Localize(ctx, "welcome?")
		utils.AssertEqual(t, "", localize)
		return fiber.NewError(500, err.Error())
	})

	app.Get("/:name", func(ctx fiber.Ctx) error {
		name := ctx.Params("name")
		localize, err := Localize(ctx, &i18n.LocalizeConfig{
			MessageID: "welcomeWithName",
			TemplateData: map[string]string{
				"name": name,
			},
		})
		utils.AssertEqual(t, nil, err)
		return ctx.SendString(localize)
	})

	t.Run("test localize", func(t *testing.T) {
		got, err := makeRequest(language.Chinese, "", app)
		utils.AssertEqual(t, 500, got.StatusCode)
		utils.AssertEqual(t, nil, err)
		body, _ := io.ReadAll(got.Body)
		utils.AssertEqual(t, `i18n.Localize error: message "welcome?" not found in language "zh"`, string(body))

		got, err = makeRequest(language.English, "name", app)
		utils.AssertEqual(t, 200, got.StatusCode)
		utils.AssertEqual(t, nil, err)
		body, _ = io.ReadAll(got.Body)
		utils.AssertEqual(t, "hello name", string(body))
	})
}

func Test_defaultLangHandler(t *testing.T) {
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(c fiber.Ctx) error {
		return c.SendString(defaultLangHandler(nil, language.English.String()))
	})
	app.Get("/test", func(c fiber.Ctx) error {
		return c.SendString(defaultLangHandler(c, language.English.String()))
	})
	t.Parallel()
	t.Run("test nil ctx", func(t *testing.T) {
		var wg sync.WaitGroup
		want := 100
		wg.Add(want)
		for i := 0; i < want; i++ {
			go func() {
				defer wg.Done()
				got, err := makeRequest(language.English, "", app)
				utils.AssertEqual(t, nil, err)
				body, _ := io.ReadAll(got.Body)
				utils.AssertEqual(t, "en", string(body))
			}()
		}
		wg.Wait()
	})

	t.Run("test query and header", func(t *testing.T) {
		got, err := makeRequest(language.Chinese, "test?lang=en", app)
		utils.AssertEqual(t, nil, err)
		body, _ := io.ReadAll(got.Body)
		utils.AssertEqual(t, "en", string(body))

		got, err = makeRequest(language.Chinese, "test", app)
		utils.AssertEqual(t, nil, err)
		body, _ = io.ReadAll(got.Body)
		utils.AssertEqual(t, "zh", string(body))

		got, err = makeRequest(language.Chinese, "test", app)
		utils.AssertEqual(t, nil, err)
		body, _ = io.ReadAll(got.Body)
		utils.AssertEqual(t, "zh", string(body))
	})
}
