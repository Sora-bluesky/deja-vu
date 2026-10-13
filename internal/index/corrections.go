package index

import (
	"strings"
	"unicode"
)

// A correction is a turn where a person told an agent it had something wrong
// — "違う、bun を使って", "no, I said the staging bucket". Those turns are the
// place a person's own knowledge gets written down without them meaning to:
// the agent assumed one thing, and they wrote out what is actually true here.
//
// The rules live in this package for the reason Friction's do: they run over
// a session being read, and later over the record log when someone asks, and
// two copies would drift. They also reuse notAsked, which knows the plumbing a
// harness writes under the user role.
//
// Nothing here claims a turn is knowledge, or that it is right. It scores how
// much a turn looks like a correction, and the weights are provisional until
// they are measured against labelled turns (docs/design/corrections.md §8).

// CorrectionSignals is what one user turn showed.
type CorrectionSignals struct {
	// Marker is the correction phrase the turn opened with or contained, from
	// the tables below; "" when none matched.
	Marker string
	// Apology is the acknowledgement the agent's next reply opened with. It is
	// weighted highest: people phrase a correction a hundred ways, agents
	// answer one in very few.
	Apology string
	// Normative reports an instruction in the turn — use, don't, always.
	Normative bool
	// Undo reports a command after the turn that put work back.
	Undo bool
	// Anger counts frustration markers, capped at 3. It is shown, never
	// ranked on: how angry someone was says nothing about whether what they
	// wrote is worth keeping.
	Anger int
	Score int
	// Accepted is the verdict: enough score, and at least one of the two
	// signals that point at a correction rather than at an ordinary request.
	Accepted bool
}

const (
	CorrectionWeightMarker    = 2
	CorrectionWeightApology   = 3
	CorrectionWeightNormative = 1
	CorrectionWeightUndo      = 1
	CorrectionMinScore        = 4

	correctionMinRunes = 8
	// A correction is written in the moment. A turn longer than this is a
	// pasted log or a spec, and shown cut off on a screen it reads as noise.
	correctionMaxRunes = 400
	// How much of the reply is searched for the acknowledgement: agents put it
	// first, and a "sorry" three paragraphs down is about something else.
	correctionApologyRunes = 160
)

// CorrectionCandidate reports whether a user turn is the kind that could be a
// correction at all. Session context — that something came before it to
// correct — is the caller's to check.
func CorrectionCandidate(text string) bool {
	t := strings.TrimSpace(text)
	if notAsked(t) {
		return false
	}
	n := len([]rune(t))
	return n >= correctionMinRunes && n <= correctionMaxRunes
}

// ScoreCorrection scores a user turn given the agent's next reply and the
// commands that ran before the next user turn.
func ScoreCorrection(user, reply string, commands []string) CorrectionSignals {
	var s CorrectionSignals
	low := strings.ToLower(strings.TrimSpace(user))
	s.Marker = correctionMarker(low)
	s.Apology = apologyMarker(reply)
	s.Normative = hasAny(low, normativeMarkers)
	for _, c := range commands {
		if hasAny(strings.ToLower(c), undoCommands) {
			s.Undo = true
			break
		}
	}
	s.Anger = angerLevel(user)
	if s.Marker != "" {
		s.Score += CorrectionWeightMarker
	}
	if s.Apology != "" {
		s.Score += CorrectionWeightApology
	}
	if s.Normative {
		s.Score += CorrectionWeightNormative
	}
	if s.Undo {
		s.Score += CorrectionWeightUndo
	}
	s.Accepted = s.Score >= CorrectionMinScore && (s.Marker != "" || s.Apology != "")
	return s
}

// The marker tables cover the languages deja already handles: Japanese,
// English, Russian. Prefix markers only count at the start of the turn,
// because "だから" or "stop" mid-sentence is ordinary prose; anywhere markers
// are specific enough to count wherever they are.
var (
	correctionPrefixMarkers = []string{
		"違う", "違います", "違くて", "ちがう", "ちゃう", "そうじゃなく", "そうではなく",
		"いや、", "いや ", "いやいや", "だから", "なんで", "何で", "ではなく",
		"no,", "no.", "no ", "nope", "wrong", "that's wrong", "that is wrong",
		"that's not", "not what i", "actually,", "wait,", "stop", "don't", "do not",
		"why did you", "why are you",
		"нет,", "нет ", "не так", "неправильно", "стоп", "зачем",
	}
	correctionAnywhereMarkers = []string{
		"じゃなくて", "ではなくて", "前も言った", "前にも言った", "さっき言った", "さっきも言った",
		"何度も", "何回も", "何回言", "勝手に", "戻して", "元に戻", "言ったよね", "言ったでしょ",
		"って言った", "余計な",
		"i said", "i told you", "i already told", "as i said", "not what i asked",
		"you broke", "revert", "undo",
		"я же говорил", "я говорил", "верни", "откати",
	}
	// Phrases a prefix marker would otherwise catch that are thanks or
	// agreement, not correction.
	correctionPrefixExceptions = []string{
		"no problem", "no worries", "いやー", "いやあ", "いやぁ",
	}
	apologyMarkers = []string{
		"申し訳", "すみません", "失礼しました", "おっしゃる通り", "おっしゃるとおり", "ご指摘", "確かに",
		"you're right", "you are right", "you're absolutely right", "you're correct",
		"sorry", "i apologize", "apologies", "good catch", "my mistake", "i misunderstood",
		"извините", "прошу прощения", "вы правы", "ты прав",
	}
	normativeMarkers = []string{
		"して", "しないで", "使って", "べき", "必ず", "絶対", "禁止", "にして", "でやって",
		" use ", "instead", "always", "never", "should", "must", "please",
		"используй", "всегда", "никогда", "надо", "нужно",
	}
	undoCommands = []string{
		"git checkout -- ", "git checkout .", "git restore", "git revert", "git reset --hard", "git stash",
	}
	angerMarkers = []string{
		"！！", "!!", "?!", "？！", "は？", "はぁ", "何回", "いい加減", "ふざけ", "まじで",
		"wtf", "what the", "fuck", "ffs", "seriously",
	}
)

func correctionMarker(low string) string {
	low = strings.TrimLeft(low, " \t\"'「『")
	for _, ex := range correctionPrefixExceptions {
		if strings.HasPrefix(low, ex) {
			return anywhereMarker(low)
		}
	}
	for _, m := range correctionPrefixMarkers {
		if strings.HasPrefix(low, m) {
			return m
		}
	}
	return anywhereMarker(low)
}

func anywhereMarker(low string) string {
	for _, m := range correctionAnywhereMarkers {
		if strings.Contains(low, m) {
			return m
		}
	}
	return ""
}

func apologyMarker(reply string) string {
	r := []rune(strings.TrimSpace(reply))
	if len(r) > correctionApologyRunes {
		r = r[:correctionApologyRunes]
	}
	low := strings.ToLower(string(r))
	for _, m := range apologyMarkers {
		if strings.Contains(low, m) {
			return m
		}
	}
	return ""
}

func hasAny(low string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(low, m) {
			return true
		}
	}
	return false
}

func angerLevel(text string) int {
	low := strings.ToLower(text)
	n := 0
	for _, m := range angerMarkers {
		if strings.Contains(low, m) {
			n++
		}
	}
	// Shouting: enough Latin letters to mean it, nearly all of them capitals.
	var letters, upper int
	for _, r := range text {
		if r < unicode.MaxASCII && unicode.IsLetter(r) {
			letters++
			if unicode.IsUpper(r) {
				upper++
			}
		}
	}
	if letters >= 8 && upper*10 >= letters*7 {
		n++
	}
	if n > 3 {
		n = 3
	}
	return n
}
