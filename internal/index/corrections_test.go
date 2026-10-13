package index

import (
	"strings"
	"testing"
)

func TestScoreCorrection(t *testing.T) {
	for _, tc := range []struct {
		name     string
		user     string
		reply    string
		commands []string
		accepted bool
	}{
		{"ja correction acknowledged", "違う、pnpm じゃなくて bun を使って", "申し訳ありません。bun で入れ直します。", nil, true},
		{"en correction acknowledged", "no, I said the staging bucket", "You're right, switching to staging.", nil, true},
		{"a marker alone does not decide", "そうじゃなくて、テストは go test ./... -race で回して", "go test を実行します。", nil, false},
		{"current models thank instead of apologising", "テストは make test でお願い", "ご指摘ありがとうございます。make test で実行します。", nil, true},
		{"not fixed needs one more signal", "まだ直ってない", "確認します。", nil, false},
		{"not fixed with an instruction is still weak", "直ってない、キャッシュを消してから試して", "確認します。", nil, false},
		{"not fixed, acknowledged", "直ってない、キャッシュを消してから試して", "ご指摘のとおりです。キャッシュを消します。", nil, true},
		{"marker, instruction and undo without an acknowledgement", "勝手に消さないで、戻して", "了解です。", []string{"git restore main.go"}, false},
		{"a question praised is not a correction", "この設計で困る点はある？", "鋭い観点です。二つあります。", nil, false},
		{"apology with instruction", "テストは make test じゃなくて go test でやって", "失礼しました。go test で実行します。", nil, true},
		// The false positives the design names.
		{"relief, not correction", "いやー助かった、ありがとう", "どういたしまして。", nil, false},
		{"agreement", "no problem, carry on with the plan", "Continuing.", nil, false},
		{"ordinary next step", "次は README を直してください", "README を更新します。", nil, false},
		{"apology far down the reply", "README も更新して", strings.Repeat("更新しました。", 30) + "すみません。", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := ScoreCorrection(tc.user, tc.reply, tc.commands)
			if s.Accepted != tc.accepted {
				t.Fatalf("accepted = %v, want %v (%+v)", s.Accepted, tc.accepted, s)
			}
		})
	}
}

func TestCorrectionCandidate(t *testing.T) {
	for _, tc := range []struct {
		text string
		want bool
	}{
		{"違う、bun を使って", true},
		{"ok", false},
		{"<system-reminder>違う、bun を使って</system-reminder>", false},
		{"This session is being continued from a previous conversation. 違う", false},
		{"違う、bun", true},
		{strings.Repeat("あ", 1001), false},
		{"Stop hook feedback: there are untracked files", false},
		{"Base directory for this skill: /x", false},
	} {
		if got := CorrectionCandidate(tc.text); got != tc.want {
			t.Errorf("CorrectionCandidate(%.30q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

func TestAngerIsNotScored(t *testing.T) {
	calm := ScoreCorrection("違う、bun を使って", "申し訳ありません。", nil)
	angry := ScoreCorrection("違う！！何回言わせるの、bun を使って", "申し訳ありません。", nil)
	if angry.Anger == 0 {
		t.Fatalf("anger not detected: %+v", angry)
	}
	if calm.Score != angry.Score {
		t.Fatalf("anger changed the score: calm %d, angry %d", calm.Score, angry.Score)
	}
}

func TestCorrectionMarkerKinds(t *testing.T) {
	for _, tc := range []struct{ text, kind string }{
		{"前も言ったけど bun を使って", CorrectionRepeat},
		{"勝手にコミットしないで", CorrectionUnasked},
		{"stop the dev server and rebuild", ""},
		{"何度も試したけどだめ", ""},
		{"いやいや、それは別のバケット", CorrectionNegation},
		{"なんで main に push したの", CorrectionWhy},
		{"冒頭の話。" + strings.Repeat("あ", 300) + "違う", ""},
	} {
		if got := ScoreCorrection(tc.text, "", nil).Kind; got != tc.kind {
			t.Errorf("%.20q: kind %q, want %q", tc.text, got, tc.kind)
		}
	}
}
