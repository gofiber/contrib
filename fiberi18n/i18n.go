package fiberi18n

import (
	"fmt"
	"path"

	"github.com/gofiber/fiber/v2"
	"github.com/nicksnyder/go-i18n/v2/i18n"
)

var appCfg *Config

// New creates a new middleware handler
func New(config ...*Config) fiber.Handler {
	appCfg = configDefault(config...)
	appCfg.bundle = initBundle()
	loadMessages()
	appCfg.localizerMap = initLocalizerMap()

	return func(c *fiber.Ctx) error {
		if appCfg.Next != nil && appCfg.Next(c) {
			return c.Next()
		}
		appCfg.ctx = c
		return c.Next()
	}
}

func initBundle() *i18n.Bundle {
	bundle := i18n.NewBundle(appCfg.DefaultLanguage)
	bundle.RegisterUnmarshalFunc(appCfg.FormatBundleFile, appCfg.UnmarshalFunc)

	return bundle
}

func loadMessage(filepath string) {
	buf, err := appCfg.Loader.LoadMessage(filepath)
	if err != nil {
		panic(err)
	}
	if _, err := appCfg.bundle.ParseMessageFileBytes(buf, filepath); err != nil {
		panic(err)
	}
}

func loadMessages() {
	for _, lang := range appCfg.AcceptLanguages {
		bundleFile := fmt.Sprintf("%s.%s", lang.String(), appCfg.FormatBundleFile)
		filepath := path.Join(appCfg.RootPath, bundleFile)

		loadMessage(filepath)
	}
}

func initLocalizerMap() map[string]*i18n.Localizer {
	localizerMap := map[string]*i18n.Localizer{}

	for _, lang := range appCfg.AcceptLanguages {
		s := lang.String()
		localizerMap[s] = i18n.NewLocalizer(appCfg.bundle, s)
	}

	lang := appCfg.DefaultLanguage.String()
	if _, ok := localizerMap[lang]; !ok {
		localizerMap[lang] = i18n.NewLocalizer(appCfg.bundle, lang)
	}

	return localizerMap
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
	var localizeConfig *i18n.LocalizeConfig

	lang := appCfg.LangHandler(appCfg.ctx, appCfg.DefaultLanguage.String())
	localizer, hasValue := appCfg.localizerMap[lang]
	if !hasValue {
		localizer = appCfg.localizerMap[appCfg.DefaultLanguage.String()]
	}

	switch paramValue := params.(type) {
	case string:
		localizeConfig = &i18n.LocalizeConfig{MessageID: paramValue}
	case *i18n.LocalizeConfig:
		localizeConfig = paramValue
	}

	message, err := localizer.Localize(localizeConfig)
	if err != nil {
		return "", fmt.Errorf("i18n.Localize error: %v", err.Error())
	}
	return message, nil
}
