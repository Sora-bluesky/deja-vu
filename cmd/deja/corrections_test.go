package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vshulcz/deja-vu/internal/index"
)

// writeCorrectionsCorpus lays down claude sessions: three in which the person
// corrects the package manager and the agent concedes, one in which the
// person corrects it and the agent just carries on, and one with no
// correction at all.
func writeCorrectionsCorpus(t *testing.T, root string) {
	t.Helper()
	proj := filepath.Join(root, "projects", "repo")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(sid string, turns ...string) {
		var rows []string
		for i, text := range turns {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			b, err := json.Marshal(map[string]any{
				"type": role, "sessionId": sid, "cwd": "/repo",
				"timestamp": fmt.Sprintf("2026-07-30T03:04:%02dZ", i),
				"message":   map[string]any{"role": role, "content": text},
			})
			if err != nil {
				t.Fatal(err)
			}
			rows = append(rows, string(b))
		}
		if err := os.WriteFile(filepath.Join(proj, sid+".jsonl"), []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for i, wording := range []string{
		"違う、このリポは pnpm じゃなくて bun を使って",
		"このリポは pnpm じゃなくて bun を使うんだって",
		"pnpm じゃなくて bun を使って、このリポは",
	} {
		write(fmt.Sprintf("conceded%d", i),
			"依存を入れて", "pnpm install で依存を入れます",
			wording, "ご指摘のとおりです。bun install で入れ直します。")
	}
	write("ignored", "テストを回して", "make test を実行します",
		"テストは go test ./... -race で回して", "go test を実行します。")
	write("plain", "README を見て", "README を確認しました",
		"次は CHANGELOG も更新してください", "CHANGELOG を更新します。")
}

func correctionsEnv(t *testing.T) {
	t.Helper()
	root := frictionEnv(t)
	writeCorrectionsCorpus(t, root)
}

func TestCorrectionsGroupsWhatWasConceded(t *testing.T) {
	correctionsEnv(t)
	var buf bytes.Buffer
	if err := runCorrections(index.DefaultDir(), nil, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, " 3 sessions") || !strings.Contains(out, "bun") {
		t.Fatalf("the three rephrasings of one correction were not grouped:\n%s", out)
	}
	if !strings.Contains(out, "agent had said: pnpm install") {
		t.Fatalf("what the agent assumed is missing:\n%s", out)
	}
	// The agent did not concede these; the default listing is only the
	// corrections it did.
	for _, absent := range []string{"go test", "CHANGELOG"} {
		if strings.Contains(out, absent) {
			t.Fatalf("an unacknowledged turn was listed (%s):\n%s", absent, out)
		}
	}
}

func TestCorrectionsCandidatesHandEveryTurnOver(t *testing.T) {
	correctionsEnv(t)
	var buf bytes.Buffer
	if err := runCorrections(index.DefaultDir(), []string{"--candidates", "--json"}, &buf); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Total int              `json:"total"`
		Turns []correctionJSON `json:"turns"`
	}
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, buf.String())
	}
	if got.Total != 5 || len(got.Turns) != 5 {
		t.Fatalf("want all 5 second turns, got total %d, %d turns", got.Total, len(got.Turns))
	}
	acknowledged := 0
	for _, turn := range got.Turns {
		if turn.Prev == "" || turn.User == "" {
			t.Fatalf("a candidate lacks its context: %+v", turn)
		}
		if turn.Acknowledged {
			acknowledged++
		}
	}
	if acknowledged != 3 {
		t.Fatalf("want 3 acknowledged, got %d", acknowledged)
	}
}

func TestCorrectionsCandidatesPage(t *testing.T) {
	correctionsEnv(t)
	var buf bytes.Buffer
	if err := runCorrections(index.DefaultDir(), []string{"--candidates", "--limit", "2", "--offset", "4"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "showing #5–#5") || strings.Contains(out, "next page") {
		t.Fatalf("the last page is wrong:\n%s", out)
	}
}

func TestCorrectionsFilters(t *testing.T) {
	correctionsEnv(t)
	var buf bytes.Buffer
	if err := runCorrections(index.DefaultDir(), []string{"--project", "no-such-project"}, &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "no acknowledged corrections") {
		t.Fatalf("want the empty state, got:\n%s", buf.String())
	}
	for _, bad := range [][]string{{"--limit", "0"}, {"--limit", "many"}, {"--offset", "-1"}, {"--since", "soon"}, {"--bogus"}, {"--project"}} {
		if err := runCorrections(index.DefaultDir(), bad, &buf); err == nil {
			t.Fatalf("%v accepted", bad)
		}
	}
}

func TestGroupCorrectionsKeepsDifferentLessonsApart(t *testing.T) {
	turn := func(text string) index.CorrectionTurn {
		return index.CorrectionTurn{User: text, Session: index.SessionMeta{ID: text, Harness: "claude"}}
	}
	groups := groupCorrections([]index.CorrectionTurn{
		turn("pnpm じゃなくて bun を使って"),
		turn("勝手に main に push しないで"),
		turn("このリポは pnpm じゃなくて bun"),
	})
	if len(groups) != 2 {
		t.Fatalf("want 2 groups, got %d", len(groups))
	}
	if len(groups[0].turns) != 2 {
		t.Fatalf("the bun correction should lead with both turns, got %d", len(groups[0].turns))
	}
}
