package envelope

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRunValidation(t *testing.T) {
	for _, tc := range []struct {
		name, run    string
		kind         Kind
		thread, want string
	}{
		{"absent", `null`, Notice, "", ""},
		{"dir notice", `{"dir":"Documents/repo"}`, Notice, "", ""},
		{"dir request", `{"dir":"~/repo"}`, Request, "", ""},
		{"desktop", `{"desktop":true}`, Notice, "", ""},
		{"both", `{"dir":"/repo","desktop":true}`, Notice, "", ""},
		{"empty", `{}`, Notice, "", "dir or desktop"},
		{"empty dir", `{"dir":""}`, Notice, "", "dir or desktop"},
		{"nul", `{"dir":"a\u0000b"}`, Notice, "", "NUL"},
		{"limit", `{"dir":"` + strings.Repeat("x", 1024) + `"}`, Notice, "", ""},
		{"bytes", `{"dir":"` + strings.Repeat("é", 513) + `"}`, Notice, "", "1024 bytes"},
		{"result", `{"dir":"/repo"}`, Result, "", "notice or request"},
		{"request desktop", `{"desktop":true}`, Request, "", "only allowed on a notice"},
		{"followup dir", `{"dir":"/repo"}`, Notice, "old", "to_thread"},
		{"followup desktop", `{"desktop":true}`, Notice, "old", "to_thread"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := Envelope{V: Version, ID: "run", Kind: tc.kind, To: "box/claude", FromAgent: "me", ToThread: tc.thread, ReplyTo: "req", Body: Body{State: Completed}}
			if err := json.Unmarshal([]byte(tc.run), &env.Run); err != nil {
				t.Fatal(err)
			}
			err := env.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}
