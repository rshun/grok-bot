package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestRedactBotToken(t *testing.T) {
	fakeToken := "1234567890:AAFakeTokenForUnitTestOnly_0123456789"
	raw := fmt.Sprintf(`Post "https://api.telegram.org/bot%s/getUpdates": unexpected EOF`, fakeToken)

	got := RedactBotToken(raw, "")
	if strings.Contains(got, fakeToken) || strings.Contains(got, "AAFakeTokenForUnitTestOnly") {
		t.Fatalf("token leaked via regex redaction: %s", got)
	}
	if got != `Post "https://api.telegram.org/bot****/getUpdates": unexpected EOF` {
		t.Fatalf("unexpected redacted text: %s", got)
	}

	gotCustom := RedactBotToken("https://example.invalid/botcustom_placeholder/getMe", "custom_placeholder")
	if strings.Contains(gotCustom, "custom_placeholder") {
		t.Fatalf("explicit token leaked: %s", gotCustom)
	}
}

func TestSafeHTTPClientSanitizesURLErrorAndPreservesUnwrap(t *testing.T) {
	fakeToken := "1234567890:AAFakeTokenForUnitTestOnly_0123456789"
	reqURL := fmt.Sprintf("https://api.telegram.org/bot%s/getUpdates", fakeToken)

	client := &safeHTTPClient{
		token: fakeToken,
		base: receiptHTTPClient{
			do: func(req *http.Request) (*http.Response, error) {
				return nil, &url.Error{
					Op:  "Post",
					URL: req.URL.String(),
					Err: io.ErrUnexpectedEOF,
				}
			},
		},
	}

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, reqURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), fakeToken) || strings.Contains(err.Error(), "AAFakeToken") {
		t.Fatalf("HTTP error leaked token: %v", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected errors.Is(..., io.ErrUnexpectedEOF), got %v", err)
	}
	var urlErr *url.Error
	if !errors.As(err, &urlErr) {
		t.Fatalf("expected *url.Error, got %T", err)
	}
	if strings.Contains(urlErr.URL, fakeToken) {
		t.Fatalf("url.Error.URL leaked token: %s", urlErr.URL)
	}
}

func TestBotLoggerRedactsTokens(t *testing.T) {
	fakeToken := "1234567890:AAFakeTokenForUnitTestOnly_0123456789"
	var logged []string
	logger := botLogger{
		warn: func(msg string, args ...any) {
			logged = append(logged, fmt.Sprint(append([]any{msg}, args...)...))
		},
	}

	logger.Println(&url.Error{
		Op:  "Post",
		URL: "https://api.telegram.org/bot" + fakeToken + "/getUpdates",
		Err: io.ErrUnexpectedEOF,
	})
	logger.Printf("Endpoint: %s, token=%s\n", "getUpdates", fakeToken)

	if len(logged) != 2 {
		t.Fatalf("expected 2 log entries, got %d", len(logged))
	}
	for _, entry := range logged {
		if strings.Contains(entry, fakeToken) || strings.Contains(entry, "AAFakeToken") {
			t.Fatalf("logger leaked token: %s", entry)
		}
	}
}
