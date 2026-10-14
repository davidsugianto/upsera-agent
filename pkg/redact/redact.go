// Package redact removes credentials and personal data from text before it is
// stored or shown to a model. It works on patterns, so it is a safety net and
// not a guarantee; callers that know a value is sensitive (such as container
// environment values) should drop it structurally instead.
package redact

import (
	"regexp"
	"strings"
	"unicode"
)

// Marker replaces every redacted value.
const Marker = "[REDACTED]"

// Result is the redacted text and how many values of each kind were removed.
type Result struct {
	Text   string
	Counts map[string]int
}

type rule struct {
	kind string
	re   *regexp.Regexp
	// repl may reference capture groups; it must keep a leading key name so the
	// output stays readable.
	repl string
}

// Order matters: the most specific shapes run first so a JWT is counted as a
// JWT and not as a generic token.
var rules = []rule{
	{"jwt", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`), Marker},
	{"bearer", regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._~+/=-]{6,}`), "${1}" + Marker},
	{"api_key", regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|AKIA[0-9A-Z]{16}|xox[baprs]-[A-Za-z0-9-]{10,})\b`), Marker},
	{"secret_pair", regexp.MustCompile(`(?i)((?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|auth)[a-z0-9_-]*["']?\s*[:=]\s*["']?)[^\s"',;&}]+`), "${1}" + Marker},
	{"email", regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)+`), Marker},
}

// ansiSeq matches a whole terminal escape sequence (CSI, OSC, or a two-byte
// ESC code), so dropping only the ESC byte cannot leave "[0m" fragments behind.
var ansiSeq = regexp.MustCompile(`\x1b(?:\[[0-9;?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)?|[@-Z\\^_])`)

// stripInvisible drops control characters (except newline and tab) and Unicode
// format characters such as zero-width spaces and bidi overrides. They are
// removed before the patterns run, because a secret split by an invisible
// character would otherwise slip past them, and they can reorder or hide text
// on a terminal. The cost is that zero-width joiners in emoji are dropped too.
func stripInvisible(s string) string {
	s = ansiSeq.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\t':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// Redact strips invisible characters, applies every rule, and reports what it
// removed. Already-redacted markers are left alone, so the function is
// idempotent.
func Redact(s string) Result {
	res := Result{Text: stripInvisible(s), Counts: map[string]int{}}
	for _, r := range rules {
		n := 0
		res.Text = r.re.ReplaceAllStringFunc(res.Text, func(m string) string {
			out := r.re.ReplaceAllString(m, r.repl)
			if out == m {
				return m
			}
			n++
			return out
		})
		if n > 0 {
			res.Counts[r.kind] = n
		}
	}
	return res
}
