package core

import (
	"reflect"
	"runtime"
	"testing"
)

func TestParseGrants(t *testing.T) {
	cases := []struct {
		text   string
		grant  []string
		revoke []string
	}{
		{"允许写入 /srv/out", []string{"/srv/out"}, nil},
		{"允许写入 /srv/out，然后读 /srv/summer", []string{"/srv/out"}, nil},
		{"允许写入 /srv/a 和 /srv/b、/srv/c", []string{"/srv/a", "/srv/b", "/srv/c"}, nil},
		{"请允许写入/srv/out目录", []string{"/srv/out"}, nil},
		{"授权写入 /srv/out的", []string{"/srv/out"}, nil},
		{"可以写入 /srv/out 里", []string{"/srv/out"}, nil},
		{"把总结写到 /srv/out", nil, nil},
		{"看看 /srv", nil, nil},
		{"取消写入 /srv/out", nil, []string{"/srv/out"}},
		{"请取消 /srv/out 的写入权限", nil, []string{"/srv/out"}},
		{"收回/srv/out的写权限。允许写入 /srv/new", []string{"/srv/new"}, []string{"/srv/out"}},
		{"允许写入 /srv/中文目录/子", []string{"/srv/中文目录/子"}, nil},
		{"允许写入\n/srv/next", nil, nil},
	}
	for _, tc := range cases {
		grant, revoke := parseGrants(tc.text)
		if !reflect.DeepEqual(grant, tc.grant) || !reflect.DeepEqual(revoke, tc.revoke) {
			t.Errorf("%q: got grant %q revoke %q, want %q %q", tc.text, grant, revoke, tc.grant, tc.revoke)
		}
	}
}

func TestMentionsPath(t *testing.T) {
	cases := map[string]bool{
		"看看 /srv":       true,
		"读一下/srv/a.txt": true,
		"你好":            false,
		"允许写入":          true,
		"1/2 是多少":       false,
	}
	for text, want := range cases {
		if got := mentionsPath(text); got != want {
			t.Errorf("%q: got %v, want %v", text, got, want)
		}
	}
}

func TestParseGrantsWindowsDrivePaths(t *testing.T) {
	if runtime.GOOS != "windows" {
		if grant, _ := parseGrants(`允许写入 C:\out`); grant != nil {
			t.Fatalf("drive path accepted off Windows: %q", grant)
		}
		return
	}
	grant, revoke := parseGrants(`允许写入 C:\Temp\out，取消写入 D:/old`)
	if len(grant) != 1 || grant[0] != `C:\Temp\out` || len(revoke) != 1 || revoke[0] != "D:/old" {
		t.Fatalf("got %q %q", grant, revoke)
	}
	if !mentionsPath(`看看 C:\Temp`) {
		t.Fatal("drive path not noticed")
	}
}
