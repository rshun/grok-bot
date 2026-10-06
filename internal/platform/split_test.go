package platform

import "testing"

func TestSplit(t *testing.T) {
	if got := Split("你好", 10); len(got) != 1 || got[0] != "你好" {
		t.Fatalf("%#v", got)
	}
	text := "aaaa\nbbbb\ncccc"
	got := Split(text, 9)
	joined := ""
	for _, part := range got {
		if len([]rune(part)) > 9 {
			t.Fatalf("chunk too long: %q", part)
		}
		if joined != "" {
			joined += "\n"
		}
		joined += part
	}
	if joined != text {
		t.Fatalf("joined %q parts %#v", joined, got)
	}
	long := make([]rune, 20)
	for i := range long {
		long[i] = '中'
	}
	got = Split(string(long), 8)
	if len(got) != 3 {
		t.Fatalf("%d parts", len(got))
	}
}
