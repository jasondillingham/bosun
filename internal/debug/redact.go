// Package debug builds the self-contained bundle that `bosun debug`
// emits for issue reports. Redact applies the heuristic
// secret-detection pass that runs on config dumps by default; the
// gather + format orchestration lives in cmd/bosun/cmd_debug.go.
package debug

import "regexp"

// secretPattern matches the common "<key>=<value>" or "<key>: <value>"
// shapes operators tend to paste into config files. Keys we recognize
// are any word containing api, secret, token, password, or key
// (case-insensitive) — so `api_key`, `API_TOKEN`, `secret-token`, and
// bare `password` all qualify. The key may be wrapped in matching
// double or single quotes (the JSON case). The capture groups
// preserve everything up to and including the operator so the caller
// can replace just the value with <redacted>.
//
// The value half intentionally captures aggressively:
//   - the inner contents of a quoted string (double or single)
//   - any non-whitespace run for unquoted forms (stopping before
//     JSON-style trailing punctuation: comma, semicolon, brace)
//
// Anything outside an obvious key=value pattern (e.g. a stray base64
// blob in free text) is NOT caught — the bundle's trailing checklist
// reminds the operator to skim for what the heuristic missed.
var secretPattern = regexp.MustCompile(
	`(?i)(["']?\b\w*(?:api|secret|token|password|key)\w*\b["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}]+)`,
)

// Redact replaces the value half of any key=value pair whose key looks
// like a secret with the literal string `<redacted>`. The key, the
// operator, and any surrounding quotes are preserved so the redacted
// output still parses as the original format (JSON, .env, YAML-ish).
//
// Operates on the entire input as a single string. Safe to call on
// arbitrary text — non-matching content is returned unchanged.
func Redact(s string) string {
	return secretPattern.ReplaceAllStringFunc(s, func(m string) string {
		sub := secretPattern.FindStringSubmatch(m)
		if len(sub) < 3 {
			return m
		}
		key, val := sub[1], sub[2]
		switch {
		case len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"':
			return key + `"<redacted>"`
		case len(val) >= 2 && val[0] == '\'' && val[len(val)-1] == '\'':
			return key + `'<redacted>'`
		default:
			return key + "<redacted>"
		}
	})
}
