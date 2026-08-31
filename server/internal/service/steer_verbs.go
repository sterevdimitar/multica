package service

import (
	"regexp"
	"strings"
)

// Verb is a control word a human can post as a comment to steer a card,
// instead of writing an ordinary comment.
type Verb string

// authorTypeMember is the only author_type that may carry a verb.
//
// The sharp edge is the converse of the obvious rule. Agent-authored
// comments cannot forge steering, which is why this gate exists - but the
// backend's and the pipeline's OWN comments are member-authored, so they
// would parse as verbs. That is why no comment this system posts may ever
// begin with "/".
const authorTypeMember = "member"

const (
	VerbNone    Verb = "" // an ordinary comment
	VerbPark    Verb = "park"
	VerbResume  Verb = "resume"
	VerbNote    Verb = "note"
	VerbUnknown Verb = "unknown" // a position-0 slash word we do not define
)

// verbPattern is the grammar: a slash, lowercase letters, then whitespace or
// end of string. It must match steer-preflight's grammar in the pipeline
// repo exactly, since ParseVerb is a port of that logic.
//
// The trailing (\s|$) is load-bearing, not decoration: it is the only reason
// a pasted path like "/src/foo.go is wrong" stays an ordinary comment. Drop
// it and every path someone pastes at position 0 becomes a control verb.
var verbPattern = regexp.MustCompile(`^/([a-z]+)(\s|$)`)

// ParseVerb reads a control verb from a human's comment. Returns the verb
// and the remaining text (a /park reason, for example).
//
// Verbs are read only from a human's comments: an agent-authored comment
// beginning with "/park" is an ordinary comment, not a verb - otherwise the
// pipeline could forge its own steering.
//
// An unknown /word is its own outcome (VerbUnknown), not VerbNone: it gets a
// reply naming the three verbs and costs no agent run, so a typo can't
// silently fall through as an instruction. Its rest is discarded.
func ParseVerb(authorType, content string) (Verb, string) {
	if authorType != authorTypeMember {
		return VerbNone, ""
	}
	match := verbPattern.FindStringSubmatch(content)
	if match == nil {
		return VerbNone, ""
	}
	word := match[1]
	rest := strings.TrimSpace(content[len("/")+len(word):])
	// Switch on Verb(word), not the raw string: the constants stay the single
	// authority for each verb's spelling.
	switch Verb(word) {
	case VerbPark:
		return VerbPark, rest
	case VerbResume:
		return VerbResume, rest
	case VerbNote:
		return VerbNote, rest
	default:
		return VerbUnknown, ""
	}
}
