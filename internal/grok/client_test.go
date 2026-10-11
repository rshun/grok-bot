package grok

import (
	"errors"
	"strings"
	"testing"
)

func TestHeadlessFailureDoesNotHideErrorBehindSessionNotice(t *testing.T) {
	const notice = "Session test-session found locally (originally in /test/old-workspace)"
	const modelError = "Error: Couldn't set model 'unsupported-model': Invalid params: unknown model id"
	tests := []struct {
		name   string
		stdout string
		stderr string
		want   string
	}{
		{"structured stdout", `{"type":"error","message":"unknown model id"}`, notice, "unknown model id"},
		{"structured stderr", "", notice + "\n" + `{"type":"error","message":"unknown model id"}`, "unknown model id"},
		{"plain stderr", "", notice + "\r\n\n" + modelError, modelError},
		{"plain stdout", modelError, notice, modelError},
		{"notice only", "", notice, "exit status 1"},
		{"ordinary failure", "", "permission denied", "permission denied"},
		{"missing session", "", "Session test-session not found", "Session test-session not found"},
		{"empty output", "", "", "exit status 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, parseErr := ParseHeadless(tt.stdout)
			err := headlessFailure(tt.stdout, tt.stderr, parseErr, errors.New("exit status 1"))
			if err == nil || err.Error() != tt.want {
				t.Fatalf("got %v, want %q", err, tt.want)
			}
			if strings.Contains(err.Error(), "/test/old-workspace") {
				t.Fatal("session lookup notice leaked into the failure")
			}
		})
	}
}

func TestHeadlessFailurePreservesParseErrorWithoutProcessError(t *testing.T) {
	_, parseErr := ParseHeadless("")
	if got := headlessFailure("", "", parseErr, nil); got != parseErr {
		t.Fatalf("got %v, want original parse error %v", got, parseErr)
	}
}
