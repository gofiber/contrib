package monitor

import (
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
)

func Test_Config_Default(t *testing.T) {
	t.Parallel()

	t.Run("use default", func(t *testing.T) {
		t.Parallel()
		cfg := configDefault()

		AssertEqual(t, defaultTitle, cfg.Title)
		AssertEqual(t, defaultRefresh, cfg.Refresh)
		AssertEqual(t, defaultFontURL, cfg.FontURL)
		AssertEqual(t, defaultChartJSURL, cfg.ChartJSURL)
		AssertEqual(t, defaultCustomHead, cfg.CustomHead)
		AssertEqual(t, false, cfg.APIOnly)
		AssertEqual(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		AssertEqual(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set title", func(t *testing.T) {
		t.Parallel()
		title := "title"
		cfg := configDefault(Config{
			Title: title,
		})

		AssertEqual(t, title, cfg.Title)
		AssertEqual(t, defaultRefresh, cfg.Refresh)
		AssertEqual(t, defaultFontURL, cfg.FontURL)
		AssertEqual(t, defaultChartJSURL, cfg.ChartJSURL)
		AssertEqual(t, defaultCustomHead, cfg.CustomHead)
		AssertEqual(t, false, cfg.APIOnly)
		AssertEqual(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		AssertEqual(t, newIndex(viewBag{title, defaultRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set refresh less than default", func(t *testing.T) {
		t.Parallel()
		cfg := configDefault(Config{
			Refresh: 100 * time.Millisecond,
		})

		AssertEqual(t, defaultTitle, cfg.Title)
		AssertEqual(t, minRefresh, cfg.Refresh)
		AssertEqual(t, defaultFontURL, cfg.FontURL)
		AssertEqual(t, defaultChartJSURL, cfg.ChartJSURL)
		AssertEqual(t, defaultCustomHead, cfg.CustomHead)
		AssertEqual(t, false, cfg.APIOnly)
		AssertEqual(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		AssertEqual(t, newIndex(viewBag{defaultTitle, minRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set refresh", func(t *testing.T) {
		t.Parallel()
		refresh := time.Second
		cfg := configDefault(Config{
			Refresh: refresh,
		})

		AssertEqual(t, defaultTitle, cfg.Title)
		AssertEqual(t, refresh, cfg.Refresh)
		AssertEqual(t, defaultFontURL, cfg.FontURL)
		AssertEqual(t, defaultChartJSURL, cfg.ChartJSURL)
		AssertEqual(t, defaultCustomHead, cfg.CustomHead)
		AssertEqual(t, false, cfg.APIOnly)
		AssertEqual(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		AssertEqual(t, newIndex(viewBag{defaultTitle, refresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set font url", func(t *testing.T) {
		t.Parallel()
		fontURL := "https://example.com"
		cfg := configDefault(Config{
			FontURL: fontURL,
		})

		AssertEqual(t, defaultTitle, cfg.Title)
		AssertEqual(t, defaultRefresh, cfg.Refresh)
		AssertEqual(t, fontURL, cfg.FontURL)
		AssertEqual(t, defaultChartJSURL, cfg.ChartJSURL)
		AssertEqual(t, defaultCustomHead, cfg.CustomHead)
		AssertEqual(t, false, cfg.APIOnly)
		AssertEqual(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		AssertEqual(t, newIndex(viewBag{defaultTitle, defaultRefresh, fontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set chart js url", func(t *testing.T) {
		t.Parallel()
		chartURL := "http://example.com"
		cfg := configDefault(Config{
			ChartJSURL: chartURL,
		})

		AssertEqual(t, defaultTitle, cfg.Title)
		AssertEqual(t, defaultRefresh, cfg.Refresh)
		AssertEqual(t, defaultFontURL, cfg.FontURL)
		AssertEqual(t, chartURL, cfg.ChartJSURL)
		AssertEqual(t, defaultCustomHead, cfg.CustomHead)
		AssertEqual(t, false, cfg.APIOnly)
		AssertEqual(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		AssertEqual(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, chartURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set custom head", func(t *testing.T) {
		t.Parallel()
		head := "head"
		cfg := configDefault(Config{
			CustomHead: head,
		})

		AssertEqual(t, defaultTitle, cfg.Title)
		AssertEqual(t, defaultRefresh, cfg.Refresh)
		AssertEqual(t, defaultFontURL, cfg.FontURL)
		AssertEqual(t, defaultChartJSURL, cfg.ChartJSURL)
		AssertEqual(t, head, cfg.CustomHead)
		AssertEqual(t, false, cfg.APIOnly)
		AssertEqual(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		AssertEqual(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, defaultChartJSURL, head}), cfg.index)
	})

	t.Run("set api only", func(t *testing.T) {
		t.Parallel()
		cfg := configDefault(Config{
			APIOnly: true,
		})

		AssertEqual(t, defaultTitle, cfg.Title)
		AssertEqual(t, defaultRefresh, cfg.Refresh)
		AssertEqual(t, defaultFontURL, cfg.FontURL)
		AssertEqual(t, defaultChartJSURL, cfg.ChartJSURL)
		AssertEqual(t, defaultCustomHead, cfg.CustomHead)
		AssertEqual(t, true, cfg.APIOnly)
		AssertEqual(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		AssertEqual(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set next", func(t *testing.T) {
		t.Parallel()
		f := func(c *fiber.Ctx) bool {
			return true
		}
		cfg := configDefault(Config{
			Next: f,
		})

		AssertEqual(t, defaultTitle, cfg.Title)
		AssertEqual(t, defaultRefresh, cfg.Refresh)
		AssertEqual(t, defaultFontURL, cfg.FontURL)
		AssertEqual(t, defaultChartJSURL, cfg.ChartJSURL)
		AssertEqual(t, defaultCustomHead, cfg.CustomHead)
		AssertEqual(t, false, cfg.APIOnly)
		AssertEqual(t, f(nil), cfg.Next(nil))
		AssertEqual(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})
}