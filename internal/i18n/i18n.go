// Package i18n provides the interface language layer for the dashboard.
//
// Languages are stored per account (users.lang) and exposed to templates
// through a Dict bound to a request. Arabic pages render right-to-left.
package i18n

import (
	"fmt"
	"strings"
)

// Lang is a supported interface language code.
type Lang string

const (
	En Lang = "en"
	Fr Lang = "fr"
	Ar Lang = "ar"
)

// Supported lists every selectable UI language.
var Supported = []Lang{En, Fr, Ar}

// Parse normalises an arbitrary language token into a supported Lang,
// defaulting to English for anything unknown or empty.
func Parse(s string) Lang {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "fr", "fr-fr", "fr_FR", "french":
		return Fr
	case "ar", "ar-ar", "ar_SA", "arabic":
		return Ar
	default:
		return En
	}
}

// String returns the language code as stored and sent to the browser.
func (l Lang) String() string { return string(l) }

// Label is the language's human name in its own script, used in the switcher.
func (l Lang) Label() string {
	switch l {
	case Fr:
		return "Français"
	case Ar:
		return "العربية"
	default:
		return "English"
	}
}

// Dir returns the document direction ("ltr" or "rtl") for this language.
func (l Lang) Dir() string {
	if l == Ar {
		return "rtl"
	}
	return "ltr"
}

// IsRTL reports whether the language renders right-to-left.
func (l Lang) IsRTL() bool { return l == Ar }

// dict holds every translation key for every language. Files underneath this
// package populate it via set() so the keys stay grouped by screen.
var dict = map[Lang]map[string]string{
	En: {},
	Fr: {},
	Ar: {},
}

// set registers a single translated string in the given language.
func set(lang Lang, key, value string) { dict[lang][key] = value }

// Dict is a language-bound lookup table handed to templates.
type Dict struct {
	lang Lang
	m    map[string]string
}

// New returns a translation table for lang. An unknown lang yields English.
func New(lang Lang) *Dict {
	if _, ok := dict[lang]; !ok {
		lang = En
	}
	return &Dict{lang: lang, m: dict[lang]}
}

// Lang returns the language this table translates into. A nil Dict is valid
// and behaves as English so templates may be rendered without injecting a
// table (e.g. in snapshot tests).
func (d *Dict) Lang() Lang {
	if d == nil {
		return En
	}
	return d.lang
}

// Dir returns the HTML dir value for the page language.
func (d *Dict) Dir() string {
	if d == nil {
		return "ltr"
	}
	return d.lang.Dir()
}

// IsRTL reports whether the page renders right-to-left.
func (d *Dict) IsRTL() bool {
	if d == nil {
		return false
	}
	return d.lang.IsRTL()
}

// T returns the translation for key in the page language. Missing keys fall
// back to English, then to the key itself so a dropped translation never
// renders as a blank hole.
func (d *Dict) T(key string) string {
	if d == nil {
		return english(key)
	}
	if v, ok := d.m[key]; ok && v != "" {
		return v
	}
	return english(key)
}

// english resolves a key against the English dictionary; a nil table can
// therefore still translate every key.
func english(key string) string {
	if v, ok := dict[En][key]; ok {
		return v
	}
	return key
}

// Tf returns a translated template string formatted with args. Values use
// fmt verbs (%d, %s) so number-heavy phrases stay grammatical per language.
func (d *Dict) Tf(key string, args ...any) string {
	return fmt.Sprintf(d.T(key), args...)
}