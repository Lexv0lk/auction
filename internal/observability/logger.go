// Package observability provides process-wide logging and metrics setup.
package observability

import (
	"io"
	"log/slog"
	"regexp"
)

// NewLogger writes structured JSON events to the supplied output stream.
// Every string and error attribute passes through RedactSecrets, and values
// carried under a credential-shaped key (password, secret, token, cookie) are
// masked regardless of their content, so a CSRF secret or a connection-string
// password can never reach the log.
func NewLogger(out io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(out, &slog.HandlerOptions{
		Level: level,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			text := attrText(attr.Value)
			if text == "" {
				return attr
			}
			if sensitiveKeyPattern.MatchString(attr.Key) {
				attr.Value = slog.StringValue("[redacted]")
			} else {
				attr.Value = slog.StringValue(RedactSecrets(text))
			}

			return attr
		},
	}))
}

// attrText flattens string and error attribute values into the text that
// reaches the log; other kinds carry nothing secret-shaped.
func attrText(value slog.Value) string {
	switch value.Kind() {
	case slog.KindString:
		return value.String()
	case slog.KindAny:
		if err, ok := value.Any().(error); ok {
			return err.Error()
		}
	}

	return ""
}

var (
	// URL credentials of the shape scheme://user:password@host.
	urlPasswordPattern = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^:/?#\s@]+:)([^@/\s]+)@`)
	// Keyword parameters of the shape password=..., used by DSN diagnostics.
	//nolint:gosec // G101 // the literal names the parameter to redact, it is not a credential
	keywordPasswordPattern = regexp.MustCompile(`(?i)\b(password|sslpassword)=([^;\s]+)`)

	// Attribute keys that carry credentials by name; their values are masked
	// no matter what the string contains.
	//nolint:gosec // G101 // the literal names the keys to redact, it is not a credential
	sensitiveKeyPattern = regexp.MustCompile(`(?i)password|secret|token|cookie|authorization`)
)

// RedactSecrets replaces the password parts of connection strings and
// password keyword parameters with [redacted]; everything else stays intact.
func RedactSecrets(s string) string {
	s = urlPasswordPattern.ReplaceAllString(s, "$1[redacted]@")

	return keywordPasswordPattern.ReplaceAllString(s, "$1=[redacted]")
}
