package envelope

import (
	"strings"
	"testing"
)

func TestFileChecks(t *testing.T) {
	hash := strings.Repeat("ab", 32)
	ok := File{Name: "report.pdf", SHA256: hash, Size: 10}
	for name, c := range map[string]struct {
		files []File
		want  string // "" = valid
	}{
		"plain":     {[]File{ok, {Name: ".env", SHA256: hash, Size: 0}}, ""},
		"directory": {[]File{{Name: "../x", SHA256: hash}}, "plain name"},
		"backslash": {[]File{{Name: `a\b`, SHA256: hash}}, "plain name"},
		"dot":       {[]File{{Name: "..", SHA256: hash}}, "bad file name"},
		"control":   {[]File{{Name: "a\nb", SHA256: hash}}, "plain name"},
		"same name": {[]File{ok, {Name: "Report.PDF", SHA256: hash}}, "two files"},
		"bad hash":  {[]File{{Name: "a", SHA256: strings.ToUpper(hash)}}, "sha256"},
		"too big":   {[]File{{Name: "a", SHA256: hash, Size: MaxFileSize + 1}}, "limit"},
		"too many":  {make([]File, MaxFiles+1), "at most"},
	} {
		env := Envelope{V: Version, ID: "01X", Kind: Notice, To: "box/worker", FromAgent: "me",
			Body: Body{Text: "x", Files: c.files}}
		err := env.Validate()
		switch {
		case c.want == "" && err != nil:
			t.Errorf("%s: %v", name, err)
		case c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)):
			t.Errorf("%s: err = %v, want %q", name, err, c.want)
		}
	}
}

func TestIDCheck(t *testing.T) {
	for id, ok := range map[string]bool{
		NewID(): true, "01REQ": true, "a-b_c": true,
		"": false, "../x": false, "a/b": false, "a.b": false, strings.Repeat("a", 65): false,
	} {
		env := Envelope{V: Version, ID: id, Kind: Notice, To: "box/worker", FromAgent: "me"}
		if err := env.Validate(); (err == nil) != ok {
			t.Errorf("id %q: err = %v", id, err)
		}
	}
}
