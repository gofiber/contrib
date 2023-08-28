package fiberi18n

import (
	"context"
	"embed"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/utils"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

//go:embed example/localizeJSON/*
var fs embed.FS

func newEmbedServer() *fiber.App {
	app := fiber.New()
	app.Use(New(&Config{
		Loader:           &EmbedLoader{fs},
		UnmarshalFunc:    json.Unmarshal,
		RootPath:         "./example/localizeJSON/",
		FormatBundleFile: "json",
	}))
	app.Get("/", func(ctx *fiber.Ctx) error {
		return ctx.SendString(MustLocalize(ctx, "welcome"))
	})
	app.Get("/:name", func(ctx *fiber.Ctx) error {
		return ctx.SendString(MustLocalize(ctx, &i18n.LocalizeConfig{
			MessageID: "welcomeWithName",
			TemplateData: map[string]string{
				"name": ctx.Params("name"),
			},
		}))
	})
	return app
}

var embedApp = newEmbedServer()

func request(lang language.Tag, name string) (*http.Response, error) {
	path := "/" + name
	req, _ := http.NewRequestWithContext(context.Background(), "GET", path, nil)
	req.Header.Add("Accept-Language", lang.String())
	req.Method = "GET"
	req.RequestURI = path
	resp, err := embedApp.Test(req)
	return resp, err
}

func TestEmbedLoader_LoadMessage(t *testing.T) {
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
				name: "",
				lang: language.Chinese,
			},
			want: "你好",
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
			got, err := request(tt.args.lang, tt.args.name)
			utils.AssertEqual(t, err, nil)
			body, err := io.ReadAll(got.Body)
			utils.AssertEqual(t, err, nil)
			utils.AssertEqual(t, tt.want, string(body))
		})
	}
}
