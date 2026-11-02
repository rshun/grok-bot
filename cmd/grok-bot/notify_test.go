package main

import (
	"strings"
	"testing"
)

func TestParsePushArgs(t *testing.T) {
	req, err := parsePushArgs([]string{"-p", "tg", "--chat", "123", "-f", "/srv/a.csv", "盘前检查", "无异常"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Platform != "telegram" || req.ChatID != "123" || len(req.Files) != 1 || req.Files[0] != "/srv/a.csv" || req.Text != "盘前检查 无异常" {
		t.Fatalf("unexpected req: %#v", req)
	}

	// Read from piped stdin when no positional text is passed.
	pipedReq, err := parsePushArgs(nil, strings.NewReader("来自管道的通知\n"), true)
	if err != nil || pipedReq.Text != "来自管道的通知" {
		t.Fatalf("piped got %#v, err %v", pipedReq, err)
	}

	// Missing both text and file fails cleanly.
	if _, err := parsePushArgs(nil, nil, false); err == nil {
		t.Fatal("expected error when both text and file are empty")
	}
}
