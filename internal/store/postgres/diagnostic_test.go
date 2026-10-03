package postgres

import "testing"

func TestDiagnosticMetadataTokens(t *testing.T) {
	t.Parallel()
	for _, token := range []string{"OutOfMemory", "SIGTERM", "node_failed", "future.reason-1"} {
		if !diagnosticToken(token) {
			t.Errorf("rejected factual token %q", token)
		}
	}
	for _, value := range []string{"", "secret=abc", "/private/path", "reason with spaces", "line\nbreak", "\x00"} {
		if diagnosticToken(value) {
			t.Errorf("accepted unstructured disclosure %q", value)
		}
	}
}
