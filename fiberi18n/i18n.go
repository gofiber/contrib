package fiberi18n

import (
	"fmt"
	"path"
	"sync"

	"github.com/gofiber/fiber/v2"
	"github.com/nicksnyder/go-i18n/v2/i18n"
)

var appCfg *Config

// New creates a new middleware handler
func New(config ...*Config) fiber.Handler {
	appCfg = configDefault(config...)
	// init bundle
	bundle := i18n.NewBundle(appCfg.DefaultLanguage)
	bundle.RegisterUnmarshalFunc(appCfg.FormatBundleFile, appCfg.UnmarshalFunc)
	appCfg.bundle = bundle

	appCfg.loadMessages().initLocalizerMap()

	return func(c *fiber.Ctx) error {
		if appCfg.Next != nil && appCfg.Next(c) {
			return c.Next()
		}

		appCfg.ctx = c

		return c.Next()
	}
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
MustGetMessage get the i18n message without error handling

	  param is one of these type: messageID, *i18n.LocalizeConfig
	  Example:
		MustGetMessage("hello") // messageID is hello
		MustGetMessage(&i18n.LocalizeConfig{
				MessageID: "welcomeWithName",
				TemplateData: map[string]string{
					"name": context.Param("name"),
				},
		})
*/
func MustGetMessage(params interface{}) string {
	message, _ := GetMessage(params)
	return message
}

/*
GetMessage get the i18n message

	 param is one of these type: messageID, *i18n.LocalizeConfig
	 Example:
		GetMessage("hello") // messageID is hello
		GetMessage(&i18n.LocalizeConfig{
				MessageID: "welcomeWithName",
				TemplateData: map[string]string{
					"name": context.Param("name"),
				},
		})
*/
func GetMessage(params interface{}) (string, error) {
	lang := appCfg.LangHandler(appCfg.ctx, appCfg.DefaultLanguage.String())

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
	}

	message, err := localizer.(*i18n.Localizer).Localize(localizeConfig)
	if err != nil {
		return "", fmt.Errorf("i18n.Localize error: %v", err)
	}
	return message, nil
}
