package i18n

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/gofiber/fiber/v3"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

func newServer(translator *I18n) *fiber.App {
	app := fiber.New()
	app.Get("/", func(ctx fiber.Ctx) error {
		return ctx.SendString(translator.MustLocalize(ctx, "welcome"))
	})
	app.Get("/:name", func(ctx fiber.Ctx) error {
		return ctx.SendString(translator.MustLocalize(ctx, &i18n.LocalizeConfig{
			MessageID: "welcomeWithName",
			TemplateData: map[string]string{
				"name": ctx.Params("name"),
			},
		}))
	})
	return app
}

var (
	sharedTranslator = New()
	i18nApp          = newServer(sharedTranslator)
)

func makeRequest(lang language.Tag, name string, app *fiber.App) (*http.Response, error) {
	path := "/" + name
	req, _ := http.NewRequestWithContext(context.Background(), "GET", path, nil)
	if lang != language.Und {
		req.Header.Add("Accept-Language", lang.String())
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
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := makeRequest(tt.args.lang, tt.args.name, i18nApp)
			assert.Equal(t, err, nil)
			body, err := io.ReadAll(got.Body)
			got.Body.Close()
			assert.Equal(t, err, nil)
			assert.Equal(t, tt.want, string(body))
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
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := makeRequest(tt.args.lang, tt.args.name, i18nApp)
			assert.Equal(t, err, nil)
			body, err := io.ReadAll(got.Body)
			got.Body.Close()
			assert.Equal(t, err, nil)
			assert.Equal(t, tt.want, string(body))
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
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := makeRequest(tt.args.lang, tt.args.name, i18nApp)
			assert.Equal(t, err, nil)
			body, err := io.ReadAll(got.Body)
			got.Body.Close()
			assert.Equal(t, err, nil)
			assert.Equal(t, tt.want, string(body))
		})
	}
}

func TestTranslatorConcurrentLocalize(t *testing.T) {
	t.Parallel()

	const workers = 64
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for i := 0; i < workers; i++ {
		lang := language.English
		if i%2 == 1 {
			lang = language.Chinese
		}
		name := fmt.Sprintf("user-%d", i)
		wg.Add(1)
		go func(lang language.Tag, name string) {
			defer wg.Done()
			resp, err := makeRequest(lang, name, i18nApp)
			if err != nil {
				errCh <- err
				return
			}
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				errCh <- err
				return
			}

			expected := fmt.Sprintf("hello %s", name)
			if lang == language.Chinese {
				expected = fmt.Sprintf("你好 %s", name)
			}
			if string(body) != expected {
				errCh <- fmt.Errorf("unexpected body %q for lang %s", string(body), lang.String())
			}
		}(lang, name)
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		assert.NoError(t, err)
	}
}

func TestLocalize(t *testing.T) {
	t.Parallel()
	translator := New()
	app := fiber.New()
	app.Get("/", func(ctx fiber.Ctx) error {
		localize, err := translator.Localize(ctx, "welcome?")
		assert.Equal(t, "", localize)
		return fiber.NewError(500, err.Error())
	})

	app.Get("/:name", func(ctx fiber.Ctx) error {
		name := ctx.Params("name")
		localize, err := translator.Localize(ctx, &i18n.LocalizeConfig{
			MessageID: "welcomeWithName",
			TemplateData: map[string]string{
				"name": name,
			},
		})
		assert.Equal(t, nil, err)
		return ctx.SendString(localize)
	})

	t.Run("test localize", func(t *testing.T) {
		got, err := makeRequest(language.Chinese, "", app)
		assert.Equal(t, 500, got.StatusCode)
		assert.Equal(t, nil, err)
		body, _ := io.ReadAll(got.Body)
		got.Body.Close()
		assert.Equal(t, `i18n.Localize error: message "welcome?" not found in language "zh"`, string(body))

		got, err = makeRequest(language.English, "name", app)
		assert.Equal(t, 200, got.StatusCode)
		assert.Equal(t, nil, err)
		body, _ = io.ReadAll(got.Body)
		got.Body.Close()
		assert.Equal(t, "hello name", string(body))
	})
}

func Test_defaultLangHandler(t *testing.T) {
	app := fiber.New()
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
				assert.Equal(t, nil, err)
				body, _ := io.ReadAll(got.Body)
				got.Body.Close()
				assert.Equal(t, "en", string(body))
			}()
		}
		wg.Wait()
	})

	t.Run("test query and header", func(t *testing.T) {
		got, err := makeRequest(language.Chinese, "test?lang=en", app)
		assert.Equal(t, nil, err)
		body, _ := io.ReadAll(got.Body)
		got.Body.Close()
		assert.Equal(t, "en", string(body))

		got, err = makeRequest(language.Chinese, "test", app)
		assert.Equal(t, nil, err)
		body, _ = io.ReadAll(got.Body)
		got.Body.Close()
		assert.Equal(t, "zh", string(body))

		got, err = makeRequest(language.Chinese, "test", app)
		assert.Equal(t, nil, err)
		body, _ = io.ReadAll(got.Body)
		got.Body.Close()
		assert.Equal(t, "zh", string(body))
	})
}
