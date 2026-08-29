package handler

import "testing"

// The denominator in the popover's "21/20" comes from here. A wrong value is
// worse than none — "21/30" reads as a run that had room left — so every
// failure path must yield 0 rather than a guess.
func TestMaxTurnsFromCustomArgs(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want int64
	}{
		{"separated form", `["--max-turns","20","--effort","high"]`, 20},
		{"joined form", `["--effort","high","--max-turns=40"]`, 40},
		{"first occurrence wins", `["--max-turns","20","--max-turns","99"]`, 20},
		{"absent", `["--effort","high"]`, 0},
		{"empty array", `[]`, 0},
		{"flag with no value", `["--effort","high","--max-turns"]`, 0},
		{"non-numeric value", `["--max-turns","lots"]`, 0},
		{"zero is not a budget", `["--max-turns","0"]`, 0},
		{"negative is not a budget", `["--max-turns","-5"]`, 0},
		{"malformed json", `{"nope":1}`, 0},
		{"not a string array", `[1,2,3]`, 0},
		{"empty bytes", ``, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := maxTurnsFromCustomArgs([]byte(c.raw)); got != c.want {
				t.Fatalf("maxTurnsFromCustomArgs(%s) = %d, want %d", c.raw, got, c.want)
			}
		})
	}
}
