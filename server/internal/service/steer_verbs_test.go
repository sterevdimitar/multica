package service

import "testing"

func TestParseVerb(t *testing.T) {
	tests := []struct {
		name       string
		authorType string
		content    string
		wantVerb   Verb
		wantRest   string
	}{
		{"park bare", "member", "/park", VerbPark, ""},
		{"park reason", "member", "/park too aggressive", VerbPark, "too aggressive"},
		{"resume", "member", "/resume", VerbResume, ""},
		{"note", "member", "/note root cause is the cache key", VerbNote, "root cause is the cache key"},
		{"typo", "member", "/pasue", VerbUnknown, ""},
		{"nonsense", "member", "/nonsense", VerbUnknown, ""},
		// The regex requires whitespace-or-end after the lowercase word, and
		// "/src/" is followed by "/", so a pasted path never matches - a
		// future "simplification" of the regex must not turn a file path
		// into a control verb.
		{"path", "member", "/src/foo.go is wrong", VerbNone, ""},
		{"mid-text", "member", "see /park docs", VerbNone, ""},
		{"agent author", "agent", "/park", VerbNone, ""},
		{"uppercase", "member", "/Park", VerbNone, ""},
		{"empty content", "member", "", VerbNone, ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotVerb, gotRest := ParseVerb(tt.authorType, tt.content)
			if gotVerb != tt.wantVerb {
				t.Errorf("ParseVerb(%q, %q) verb = %q, want %q", tt.authorType, tt.content, gotVerb, tt.wantVerb)
			}
			if gotRest != tt.wantRest {
				t.Errorf("ParseVerb(%q, %q) rest = %q, want %q", tt.authorType, tt.content, gotRest, tt.wantRest)
			}
		})
	}
}
