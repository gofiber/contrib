package fiberi18n

import (
	"context"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

func newServer() *fiber.App {
	app := fiber.New()
	app.Use(New())
	app.Get("/", func(ctx *fiber.Ctx) error {
		return ctx.SendString(MustGetMessage("welcome"))
	})
	app.Get("/:name", func(ctx *fiber.Ctx) error {
		return ctx.SendString(MustGetMessage(&i18n.LocalizeConfig{
			MessageID: "welcomeWithName",
			TemplateData: map[string]string{
				"name": ctx.Params("name"),
			},
		}))
	})
	return app
}

var i18nApp = newServer()

func makeRequest(lang language.Tag, name string) (*http.Response, error) {
	path := "/" + name
	req, _ := http.NewRequestWithContext(context.Background(), "GET", path, nil)
	req.Header.Add("Accept-Language", lang.String())
	req.Method = "GET"
	req.RequestURI = path
	resp, err := i18nApp.Test(req)
	return resp, err
}

func TestI18nEN(t *testing.T) {
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
			got, err := makeRequest(tt.args.lang, tt.args.name)
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
			got, err := makeRequest(tt.args.lang, tt.args.name)
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
			got, err := makeRequest(tt.args.lang, tt.args.name)
			utils.AssertEqual(t, err, nil)
			body, err := io.ReadAll(got.Body)
			utils.AssertEqual(t, err, nil)
			utils.AssertEqual(t, tt.want, string(body))
		})
	}
}
