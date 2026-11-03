package memory

import "regexp"

// The patterns deliberately target secret-shaped values, not personal content.
// A memory store that strips ordinary prose is as harmful as one that keeps a
// token: both make the record untrustworthy.
var redactions = []struct {
	re   *regexp.Regexp
	with string
}{
	// PEM private keys.
	{regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`), "[redacted-private-key]"},
	// Header-style credentials: password=..., api_key: ...
	{regexp.MustCompile(`(?i)\b(api[_-]?key|access[_-]?token|auth[_-]?token|secret|password|passwd|client[_-]?secret|private[_-]?key)\b\s*[:=]\s*[^\s,;]+`), "$1=[redacted]"},
	// Bearer and basic credentials.
	{regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]{8,}`), "Bearer [redacted]"},
	// Common provider key shapes.
	{regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{16,}`), "[redacted-key]"},
	{regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{16,}`), "[redacted-key]"},
	{regexp.MustCompile(`\bAKIA[0-9A-Z]{16}\b`), "[redacted-key]"},
	{regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{10,}`), "[redacted-key]"},
	// Credentials embedded in a URI.
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/@\s]+@`), "$1[redacted]@"},
}

// Redact removes secret-shaped values before a record is persisted. It is
// intentionally idempotent: running it twice produces the same text.
func Redact(text string) string {
	for _, rule := range redactions {
		text = rule.re.ReplaceAllString(text, rule.with)
	}
	return text
}
