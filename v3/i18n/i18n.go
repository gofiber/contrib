package i18n

import (
	"fmt"
	"path"
	"sync"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/log"
	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"
)

// I18n exposes thread-safe localization helpers backed by a shared bundle
// and localizer map. Use New to construct an instance during application start
// and reuse it across handlers.
type I18n struct {
	cfg *Config
}

// New prepares a thread-safe i18n container instance.
func New(config ...*Config) *I18n {
	cfg := prepareConfig(config...)

	return &I18n{cfg: cfg}
}

func prepareConfig(config ...*Config) *Config {
	source := configDefault(config...)

	cfg := *source

	if source.AcceptLanguages != nil {
		cfg.AcceptLanguages = append([]language.Tag(nil), source.AcceptLanguages...)
	}

	bundle := i18n.NewBundle(cfg.DefaultLanguage)
	bundle.RegisterUnmarshalFunc(cfg.FormatBundleFile, cfg.UnmarshalFunc)
	cfg.bundle = bundle

	cfg.loadMessages()
	cfg.initLocalizerMap()

	return &cfg
}

func (c *Config) loadMessage(filepath string) {
	buf, err := c.Loader.LoadMessage(filepath)
	if err != nil {
		panic(err)
	}
	if _, err := c.bundle.ParseMessageFileBytes(buf, filepath); err != nil {
		panic(err)
	}
}

func (c *Config) loadMessages() *Config {
	for _, lang := range c.AcceptLanguages {
		bundleFilePath := fmt.Sprintf("%s.%s", lang.String(), c.FormatBundleFile)
		filepath := path.Join(c.RootPath, bundleFilePath)
		c.loadMessage(filepath)
	}
	return c
}

func (c *Config) initLocalizerMap() {
	localizerMap := &sync.Map{}

	for _, lang := range c.AcceptLanguages {
		s := lang.String()
		localizerMap.Store(s, i18n.NewLocalizer(c.bundle, s))
	}

	lang := c.DefaultLanguage.String()
	if _, ok := localizerMap.Load(lang); !ok {
		localizerMap.Store(lang, i18n.NewLocalizer(c.bundle, lang))
	}
	c.localizerMap = localizerMap
}

/*
MustLocalize get the i18n message without error handling

	  param is one of these type: messageID, *i18n.LocalizeConfig
	  Example:
		MustLocalize(ctx, "hello") // messageID is hello
		MustLocalize(ctx, &i18n.LocalizeConfig{
				MessageID: "welcomeWithName",
				TemplateData: map[string]string{
					"name": context.Param("name"),
				},
		})
*/
func (i *I18n) MustLocalize(ctx fiber.Ctx, params interface{}) string {
	message, err := i.Localize(ctx, params)
	if err != nil {
		panic(err)
	}
	return message
}

/*
Localize get the i18n message

	 param is one of these type: messageID, *i18n.LocalizeConfig
	 Example:
		Localize(ctx, "hello") // messageID is hello
		Localize(ctx, &i18n.LocalizeConfig{
				MessageID: "welcomeWithName",
				TemplateData: map[string]string{
					"name": context.Param("name"),
				},
		})
*/
func (i *I18n) Localize(ctx fiber.Ctx, params interface{}) (string, error) {
	if i == nil || i.cfg == nil {
		return "", fmt.Errorf("i18n.Localize error: %v", "translator is nil")
	}

	appCfg := i.cfg
	lang := appCfg.LangHandler(ctx, appCfg.DefaultLanguage.String())
	localizer, _ := appCfg.localizerMap.Load(lang)

	if localizer == nil {
		defaultLang := appCfg.DefaultLanguage.String()
		localizer, _ = appCfg.localizerMap.Load(defaultLang)
	}

	var localizeConfig *i18n.LocalizeConfig
	switch paramValue := params.(type) {
	case string:
		localizeConfig = &i18n.LocalizeConfig{MessageID: paramValue}
	case *i18n.LocalizeConfig:
		localizeConfig = paramValue
	default:
		return "", fmt.Errorf("i18n.Localize error: %v", "unsupported params type")
	}

	if localizer == nil {
		return "", fmt.Errorf("i18n.Localize error: %v", "localizer is nil")
	}

	message, err := localizer.(*i18n.Localizer).Localize(localizeConfig)
	if err != nil {
		log.Errorf("i18n.Localize error: %v", err)
		return "", fmt.Errorf("i18n.Localize error: %v", err)
	}
	return message, nil
}
