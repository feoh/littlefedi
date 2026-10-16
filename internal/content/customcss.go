package content

import (
	"errors"
	"strings"
	"unicode/utf8"
)

// MaxCustomCSSBytes caps an account's own stylesheet. It is inlined into every
// page that account loads, so this is a per-request page-weight ceiling; the
// largest wired-in theme is well under it.
const MaxCustomCSSBytes = 64 << 10

// Reasons ValidateCustomCSS can reject a stylesheet. Callers map these onto
// distinct messages, so they are values rather than one opaque error.
var (
	ErrCustomCSSTooLarge = errors.New("custom css too large")
	ErrCustomCSSMarkup   = errors.New("custom css contains markup")
	ErrCustomCSSEncoding = errors.New("custom css is not valid utf-8")
)

// ValidateCustomCSS trims and checks account-supplied CSS, returning the value
// to store. It is not a CSS parser: browsers skip declarations
// they cannot make sense of, so malformed CSS only breaks the page of whoever
// wrote it. What it does reject is markup, because the stylesheet is inlined
// into a <style> element, and "</" or "<!--" are the only sequences that can
// end that element early and turn styling into script injection.
func ValidateCustomCSS(css string) (string, error) {
	css = strings.TrimSpace(css)
	if css == "" {
		return "", nil
	}
	if len(css) > MaxCustomCSSBytes {
		return "", ErrCustomCSSTooLarge
	}
	if !utf8.ValidString(css) {
		return "", ErrCustomCSSEncoding
	}
	if strings.Contains(css, "</") || strings.Contains(css, "<!--") {
		return "", ErrCustomCSSMarkup
	}
	return css, nil
}
