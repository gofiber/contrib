package monitor

import (
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/assert"
)

func Test_Config_Default(t *testing.T) {
	t.Parallel()

	t.Run("use default", func(t *testing.T) {
		t.Parallel()
		cfg := configDefault()

		assert.Equal(t, defaultTitle, cfg.Title)
		assert.Equal(t, defaultRefresh, cfg.Refresh)
		assert.Equal(t, defaultFontURL, cfg.FontURL)
		assert.Equal(t, defaultChartJSURL, cfg.ChartJSURL)
		assert.Equal(t, defaultCustomHead, cfg.CustomHead)
		assert.Equal(t, false, cfg.APIOnly)
		assert.IsType(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		assert.Equal(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set title", func(t *testing.T) {
		t.Parallel()
		title := "title"
		cfg := configDefault(Config{
			Title: title,
		})

		assert.Equal(t, title, cfg.Title)
		assert.Equal(t, defaultRefresh, cfg.Refresh)
		assert.Equal(t, defaultFontURL, cfg.FontURL)
		assert.Equal(t, defaultChartJSURL, cfg.ChartJSURL)
		assert.Equal(t, defaultCustomHead, cfg.CustomHead)
		assert.Equal(t, false, cfg.APIOnly)
		assert.IsType(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		assert.Equal(t, newIndex(viewBag{title, defaultRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set refresh less than default", func(t *testing.T) {
		t.Parallel()
		cfg := configDefault(Config{
			Refresh: 100 * time.Millisecond,
		})

		assert.Equal(t, defaultTitle, cfg.Title)
		assert.Equal(t, minRefresh, cfg.Refresh)
		assert.Equal(t, defaultFontURL, cfg.FontURL)
		assert.Equal(t, defaultChartJSURL, cfg.ChartJSURL)
		assert.Equal(t, defaultCustomHead, cfg.CustomHead)
		assert.Equal(t, false, cfg.APIOnly)
		assert.IsType(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		assert.Equal(t, newIndex(viewBag{defaultTitle, minRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set refresh", func(t *testing.T) {
		t.Parallel()
		refresh := time.Second
		cfg := configDefault(Config{
			Refresh: refresh,
		})

		assert.Equal(t, defaultTitle, cfg.Title)
		assert.Equal(t, refresh, cfg.Refresh)
		assert.Equal(t, defaultFontURL, cfg.FontURL)
		assert.Equal(t, defaultChartJSURL, cfg.ChartJSURL)
		assert.Equal(t, defaultCustomHead, cfg.CustomHead)
		assert.Equal(t, false, cfg.APIOnly)
		assert.IsType(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		assert.Equal(t, newIndex(viewBag{defaultTitle, refresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set font url", func(t *testing.T) {
		t.Parallel()
		fontURL := "https://example.com"
		cfg := configDefault(Config{
			FontURL: fontURL,
		})

		assert.Equal(t, defaultTitle, cfg.Title)
		assert.Equal(t, defaultRefresh, cfg.Refresh)
		assert.Equal(t, fontURL, cfg.FontURL)
		assert.Equal(t, defaultChartJSURL, cfg.ChartJSURL)
		assert.Equal(t, defaultCustomHead, cfg.CustomHead)
		assert.Equal(t, false, cfg.APIOnly)
		assert.IsType(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		assert.Equal(t, newIndex(viewBag{defaultTitle, defaultRefresh, fontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set chart js url", func(t *testing.T) {
		t.Parallel()
		chartURL := "http://example.com"
		cfg := configDefault(Config{
			ChartJSURL: chartURL,
		})

		assert.Equal(t, defaultTitle, cfg.Title)
		assert.Equal(t, defaultRefresh, cfg.Refresh)
		assert.Equal(t, defaultFontURL, cfg.FontURL)
		assert.Equal(t, chartURL, cfg.ChartJSURL)
		assert.Equal(t, defaultCustomHead, cfg.CustomHead)
		assert.Equal(t, false, cfg.APIOnly)
		assert.IsType(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		assert.Equal(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, chartURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set custom head", func(t *testing.T) {
		t.Parallel()
		head := "head"
		cfg := configDefault(Config{
			CustomHead: head,
		})

		assert.Equal(t, defaultTitle, cfg.Title)
		assert.Equal(t, defaultRefresh, cfg.Refresh)
		assert.Equal(t, defaultFontURL, cfg.FontURL)
		assert.Equal(t, defaultChartJSURL, cfg.ChartJSURL)
		assert.Equal(t, head, cfg.CustomHead)
		assert.Equal(t, false, cfg.APIOnly)
		assert.IsType(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		assert.Equal(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, defaultChartJSURL, head}), cfg.index)
	})

	t.Run("set api only", func(t *testing.T) {
		t.Parallel()
		cfg := configDefault(Config{
			APIOnly: true,
		})

		assert.Equal(t, defaultTitle, cfg.Title)
		assert.Equal(t, defaultRefresh, cfg.Refresh)
		assert.Equal(t, defaultFontURL, cfg.FontURL)
		assert.Equal(t, defaultChartJSURL, cfg.ChartJSURL)
		assert.Equal(t, defaultCustomHead, cfg.CustomHead)
		assert.Equal(t, true, cfg.APIOnly)
		assert.IsType(t, (func(*fiber.Ctx) bool)(nil), cfg.Next)
		assert.Equal(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})

	t.Run("set next", func(t *testing.T) {
		t.Parallel()
		f := func(c *fiber.Ctx) bool {
			return true
		}
		cfg := configDefault(Config{
			Next: f,
		})

		assert.Equal(t, defaultTitle, cfg.Title)
		assert.Equal(t, defaultRefresh, cfg.Refresh)
		assert.Equal(t, defaultFontURL, cfg.FontURL)
		assert.Equal(t, defaultChartJSURL, cfg.ChartJSURL)
		assert.Equal(t, defaultCustomHead, cfg.CustomHead)
		assert.Equal(t, false, cfg.APIOnly)
		assert.Equal(t, f(nil), cfg.Next(nil))
		assert.Equal(t, newIndex(viewBag{defaultTitle, defaultRefresh, defaultFontURL, defaultChartJSURL, defaultCustomHead}), cfg.index)
	})
}