package render

import (
	"os"
	"strings"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/styles"
)

// ColorsEnabled returns whether terminal colors should be used.
// It returns false if the NO_COLOR environment variable is set (any value)
// or if TERM is set to "dumb".
func ColorsEnabled() bool {
	if _, ok := os.LookupEnv("NO_COLOR"); ok {
		return false
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	return true
}

// RenderMarkdown renders markdown text for terminal display.
// When colors are disabled, it returns the content unmodified.
func RenderMarkdown(content string) (string, error) {
	if content == "" {
		return "", nil
	}

	if !ColorsEnabled() {
		return content, nil
	}

	var rendered string
	var err error
	if os.Getenv("GLAMOUR_STYLE") != "" {
		rendered, err = glamour.RenderWithEnvironmentConfig(content)
	} else {
		style := styles.DarkStyleConfig
		style.Document.Color = nil
		renderer, rendererErr := glamour.NewTermRenderer(glamour.WithStyles(style))
		if rendererErr != nil {
			return content, rendererErr
		}
		rendered, err = renderer.Render(content)
	}
	if err != nil {
		return content, err
	}

	// glamour pads every line out to its wrap width; strip that padding so a
	// rendered comment body carries no trailing whitespace.
	return trimTrailingSpace(strings.TrimSpace(rendered)), nil
}
