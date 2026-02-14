package render

import (
	"os"
	"strings"

	"github.com/charmbracelet/glamour"
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

	rendered, err := glamour.RenderWithEnvironmentConfig(content)
	if err != nil {
		return content, err
	}

	return strings.TrimSpace(rendered), nil
}
