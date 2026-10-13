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
	// Kind and Marker are the kind of correction and the phrase that showed
	// it, from the tables below; both "" when none matched.
	Kind   string
	Marker string
	// Apology is the acknowledgement the agent's next reply opened with. It is
	// weighted highest: people phrase a correction a hundred ways, agents
	// answer one in few. Measured on one store, current models rarely say
	// sorry — "ご指摘ありがとうございます" outnumbered "申し訳" thirty to one.
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
	// Accepted is the verdict: the agent's next reply acknowledged a mistake.
	Accepted bool
}

const (
	CorrectionWeightApology   = 3
	CorrectionWeightNormative = 1
	CorrectionWeightUndo      = 1

	// "違う、bun" is four runes and a whole correction.
	correctionMinRunes = 4
	// Corrections come with context more often than not; the first run cut
	// at 400 runes and dropped two thirds of all user turns. Past this length
	// a turn is a pasted log or a spec.
	correctionMaxRunes = 1000
	// The markers are looked for in the opening of the turn: a correction
	// says so first, and a "違う" in the eighth paragraph of a spec is quoted.
	correctionHeadRunes = 300
	// How much of the reply is searched for the acknowledgement: agents put it
	// first, and a "sorry" three paragraphs down is about something else.
	correctionApologyRunes = 160
)

// Correction kinds, with what a marker of each kind adds to the score. The
// three that say outright the agent got it wrong are enough on their own; a
// report that something is still broken, or a request to put work back, is a
// correction of a claim rather than of knowledge and needs one more signal.
const (
	CorrectionNegation = "negation"
	CorrectionRepeat   = "repeat"
	CorrectionUnasked  = "unasked"
	CorrectionNotFixed = "not-fixed"
	CorrectionUndo     = "undo"
	CorrectionWhy      = "why"
)

var CorrectionKindWeight = map[string]int{
	CorrectionNegation: 3, CorrectionRepeat: 3, CorrectionUnasked: 3,
	CorrectionNotFixed: 2, CorrectionUndo: 1, CorrectionWhy: 1,
}

// CorrectionReject says why a user turn cannot be a correction at all —
// "plumbing", "short", "long" — or "" when it can. Session context — that
// something came before it to correct — is the caller's to check.
func CorrectionReject(text string) string {
	t := strings.TrimSpace(text)
	if notAsked(t) || correctionPlumbing(t) {
		return "plumbing"
	}
	n := len([]rune(t))
	switch {
	case n < correctionMinRunes:
		return "short"
	case n > correctionMaxRunes:
		return "long"
	}
	return ""
}

// CorrectionCandidate reports whether a user turn could be a correction.
func CorrectionCandidate(text string) bool { return CorrectionReject(text) == "" }

// correctionPlumbing covers what harnesses write under the user role that
// notAsked does not need to know about. "Stop hook feedback:" was the most
// frequent marker hit on the first real run, all of it a hook talking.
func correctionPlumbing(t string) bool {
	low := strings.ToLower(t)
	for _, p := range []string{
		"stop hook feedback", "pretooluse", "posttooluse", "hook ", "base directory for this skill",
		"<", "[image", "[pasted",
	} {
		if strings.HasPrefix(low, p) {
			return true
		}
	}
	return false
}

// ScoreCorrection scores a user turn given the agent's next reply and the
// commands that ran before the next user turn.
func ScoreCorrection(user, reply string, commands []string) CorrectionSignals {
	var s CorrectionSignals
	low := strings.ToLower(headRunes(strings.TrimSpace(user), correctionHeadRunes))
	s.Kind, s.Marker = correctionMarker(low)
	s.Apology = apologyMarker(reply)
	s.Normative = hasAny(low, normativeMarkers)
	for _, c := range commands {
		if hasAny(strings.ToLower(c), undoCommands) {
			s.Undo = true
			break
		}
	}
	s.Anger = angerLevel(user)
	s.Score += CorrectionKindWeight[s.Kind]
	if s.Apology != "" {
		s.Score += CorrectionWeightApology
	}
	if s.Normative {
		s.Score += CorrectionWeightNormative
	}
	if s.Undo {
		s.Score += CorrectionWeightUndo
	}
	// Only the agent's acknowledgement held up on a fresh sample: 90% on the
	// 300 turns the tables were tuned on and 82% on 100 new ones, where the
	// person's own markers fell to 40%. People correct an agent in their own
	// words; agents concede in a few stock phrases. Markers still rank and
	// name the kind, they do not decide.
	s.Accepted = s.Apology != ""
	return s
}

type markerTable struct {
	kind             string
	prefix, anywhere []string
}

// The marker tables cover the languages deja already handles: Japanese,
// English, Russian. Prefix markers only count at the start of the turn,
// because "だから" or "stop" mid-sentence is ordinary prose; anywhere markers
// are specific enough to count wherever they are in the opening. Order
// matters: the first kind that matches names the turn.
var correctionTables = []markerTable{
	{CorrectionNegation,
		[]string{"違う", "違います", "違くて", "ちがう", "ちゃう", "そうじゃな", "そうではな", "いや、", "いや ",
			"いやいや", "ではなく", "じゃなく", "no,", "no.", "nope", "wrong", "that's wrong", "that is wrong",
			"that's not", "not what i", "actually,", "нет,", "не так", "неправильно"},
		// "ではなく" anywhere was added from fragments of the first labelled
		// sample, where one turn sampled seven times carried it; on a fresh
		// sample it was a correction in 11 of 23 turns.
		[]string{"じゃなくて", "ではなくて", "じゃなく、", "そうじゃない", "そういう意味じゃ", "意味が違", "間違って",
			"間違い", "not what i asked", "that's not what", "неправильно"}},
	{CorrectionRepeat, nil,
		[]string{"前も言った", "前にも言った", "さっき言った", "さっきも言った", "何度も言", "何回も言", "何回言",
			"言ったよね", "言ったでしょ", "言ったのに", "また同じ", "i said", "i told you", "i already told",
			"as i said", "я же говорил", "я говорил"}},
	{CorrectionUnasked,
		// "stop." and "stop," were 0 of 2 on the first labelled run: an
		// operator session telling a worker to halt, not a correction.
		[]string{"don't", "do not", "why did you"},
		[]string{"勝手に", "頼んでない", "頼んでいない", "求めてない", "余計な", "しなくていい", "しなくて良い",
			"いらない", "不要です", "やめて", "i didn't ask", "no need to", "stop doing", "зачем"}},
	{CorrectionNotFixed, nil,
		[]string{"直ってない", "直っていない", "治ってない", "変わってない", "変わっていない", "反映されてない",
			"反映されていない", "動かない", "動いてない", "できてない", "できていない", "まだ同じ",
			"still broken", "still not", "doesn't work", "didn't work", "not working", "still failing"}},
	{CorrectionUndo, nil,
		[]string{"戻して", "元に戻", "やり直", "revert", "undo", "верни", "откати"}},
	{CorrectionWhy,
		[]string{"なんで", "何で", "なぜ", "どうして", "why are you", "why is"}, nil},
}

var (
	// Phrases a prefix marker would otherwise catch that are thanks or
	// agreement, not correction.
	correctionPrefixExceptions = []string{
		"no problem", "no worries", "いやー", "いやあ", "いやぁ",
	}
	apologyMarkers = []string{
		"申し訳", "すみません", "失礼しました", "失礼いたしました", "ごめんなさい", "おっしゃる通り", "おっしゃるとおり",
		// "鋭い" was added from the tuning sample and was a correction in 1
		// of 4 fresh turns: agents also call a question "鋭い観点".
		"ご指摘", "確かに", "そのとおりです", "その通りです",
		// "誤り" and "見落と" alone were 60% and 50% on the labelled run: an
		// agent also writes "誤りはありません" and "見落としがないか確認します".
		// Only the forms that admit one count.
		"誤りでした", "誤っていました", "誤りがありました", "見落としていました", "見落としがありました",
		"見落としました", "間違えて", "間違っていました", "勘違い", "確認不足", "認識違い",
		"you're right", "you are right", "you're absolutely right", "you're correct",
		"sorry", "i apologize", "apologies", "good catch", "my mistake", "i misunderstood", "i was wrong",
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

func correctionMarker(low string) (kind, marker string) {
	low = strings.TrimLeft(low, " \t\"'「『")
	prefixOK := true
	for _, ex := range correctionPrefixExceptions {
		if strings.HasPrefix(low, ex) {
			prefixOK = false
			break
		}
	}
	for _, t := range correctionTables {
		if prefixOK {
			for _, m := range t.prefix {
				if strings.HasPrefix(low, m) {
					return t.kind, m
				}
			}
		}
		for _, m := range t.anywhere {
			if strings.Contains(low, m) {
				return t.kind, m
			}
		}
	}
	return "", ""
}

func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func apologyMarker(reply string) string {
	low := strings.ToLower(headRunes(strings.TrimSpace(reply), correctionApologyRunes))
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
