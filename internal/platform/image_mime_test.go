package platform

import "testing"

func TestImageMIME(t *testing.T) {
	cases := map[string]string{
		"image/jpeg":               "image/jpeg",
		"IMAGE/PNG":                "image/png",
		" image/webp; charset=x ":  "image/webp",
		"image/jpg":                "image/jpeg",
		"image/gif":                "image/gif",
		"image/svg+xml":            "",
		"application/octet-stream": "",
		"":                         "",
	}
	for in, want := range cases {
		if got := ImageMIME(in); got != want {
			t.Errorf("ImageMIME(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestImageMIMEByName(t *testing.T) {
	cases := map[string]string{
		"a.JPG": "image/jpeg", "b.jpeg": "image/jpeg", "c.png": "image/png",
		"d.gif": "image/gif", "e.webp": "image/webp", "f.svg": "", "noext": "",
	}
	for in, want := range cases {
		if got := ImageMIMEByName(in); got != want {
			t.Errorf("ImageMIMEByName(%q) = %q, want %q", in, got, want)
		}
	}
}
