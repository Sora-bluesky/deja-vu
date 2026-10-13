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
		{"marker and instruction, no apology", "そうじゃなくて、テストは go test ./... -race で回して", "go test を実行します。", nil, false},
		{"marker, instruction and undo", "勝手に消さないで、戻して", "了解です。", []string{"git restore main.go"}, true},
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
		{strings.Repeat("あ", 401), false},
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
