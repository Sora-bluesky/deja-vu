// Command correctionsprobe measures the correction detector in
// internal/index/corrections.go against a real index, before anything is
// built on it (docs/design/corrections.md §8).
//
//	go run ./scripts/correctionsprobe sample [-o file] [-n 300] [-top 20] [-seed 1]
//	go run ./scripts/correctionsprobe label  [-o file]
//	go run ./scripts/correctionsprobe eval   [-o file]
//
// sample writes candidate turns, with the turn before and after, to a JSONL
// file outside the repository: it is someone's private history and must never
// be committed. label walks that file in the terminal. eval prints counts and
// rates only — never corpus text — so its output is safe to paste anywhere.
//
// The sample is stratified so both precision and misses can be estimated:
// the top-scoring accepted turns (what a screen would show first), a random
// draw from the rest of the accepted ones, and a random draw from the
// candidates the detector rejected.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/vshulcz/deja-vu/internal/index"
)

const (
	stratumTop      = "top"
	stratumAccepted = "accepted"
	stratumRejected = "rejected"
)

type meta struct {
	Index      string         `json:"index"`
	SampledAt  time.Time      `json:"sampled_at"`
	Candidates int            `json:"candidates"`
	Population map[string]int `json:"population"`
	Seed       int64          `json:"seed"`
}

type item struct {
	ID       int       `json:"id"`
	Stratum  string    `json:"stratum"`
	Harness  string    `json:"harness"`
	Project  string    `json:"project"`
	Time     time.Time `json:"time"`
	Prev     string    `json:"prev"`
	PrevCmd  string    `json:"prev_cmd,omitempty"`
	User     string    `json:"user"`
	Reply    string    `json:"reply"`
	Kind     string    `json:"kind,omitempty"`
	Marker   string    `json:"marker,omitempty"`
	Apology  string    `json:"apology,omitempty"`
	Norm     bool      `json:"normative,omitempty"`
	Undo     bool      `json:"undo,omitempty"`
	Anger    int       `json:"anger,omitempty"`
	Score    int       `json:"score"`
	Accepted bool      `json:"accepted"`
	// Label: d (correction carrying domain knowledge), w (correction about how
	// the agent works), n (not a correction), u (unsure). Empty until labelled.
	Label string `json:"label,omitempty"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	cmd := os.Args[1]
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	home, _ := os.UserHomeDir()
	out := fs.String("o", filepath.Join(home, "deja-corrections-sample.jsonl"), "sample file (keep it outside any repository)")
	n := fs.Int("n", 300, "turns to sample in total")
	top := fs.Int("top", 20, "highest-scoring accepted turns always included")
	seed := fs.Int64("seed", 1, "random seed")
	features := fs.Bool("features", false, "eval: list what separates corrections from the rest (prints agent reply openings and 2-3 character fragments of your turns)")
	openings := fs.Bool("openings", false, "stats: also list how agent replies open after a correction marker (prints agent text, not yours)")
	_ = fs.Parse(os.Args[2:])
	var err error
	switch cmd {
	case "sample":
		err = sample(*out, *n, *top, *seed)
	case "label":
		err = label(*out)
	case "eval":
		err = eval(*out, *features)
	case "stats":
		err = stats(*openings)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "correctionsprobe:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: correctionsprobe stats|sample|label|eval [-o file]")
	os.Exit(2)
}

// funnel counts where user turns drop out before scoring, so a low yield can
// be traced to the filter that caused it.
type funnel struct {
	userTurns, first, plumbing, short, long, automated, nothingBefore int
}

// automatedSessions is how many separate sessions the same user text must
// appear in before it is taken for a scripted prompt rather than a person:
// the first real run had one instruction, sent verbatim by a loop, as the
// most frequent "correction".
const automatedSessions = 5

// collect reads the record log once, rebuilds each session's turn order, and
// scores every user turn that has something before it to correct.
func collect() (string, []item, funnel, error) {
	var f funnel
	dir := index.DefaultDir()
	recs, err := index.ReadRecords(dir)
	if err != nil {
		return dir, nil, f, fmt.Errorf("read %s: %w", dir, err)
	}
	metas, err := index.AllMeta(dir)
	if err != nil {
		return dir, nil, f, err
	}
	byKey := map[string]index.SessionMeta{}
	for _, m := range metas {
		byKey[m.Harness+":"+m.ID] = m
	}
	// A session is written in message order, and an incremental update
	// rewrites a changed session whole, so offset order within a key is turn
	// order. Records of different sessions can interleave; grouping undoes it.
	sessions := map[string][]index.OffsetRecord{}
	for _, r := range recs {
		switch r.Record.Role {
		case "user", "assistant", "tool-output", "command":
			sessions[r.Record.Key] = append(sessions[r.Record.Key], r)
		}
	}
	seenIn := map[string]map[string]bool{}
	for key, rs := range sessions {
		for _, r := range rs {
			if r.Record.Role != "user" {
				continue
			}
			t := strings.TrimSpace(r.Record.Text)
			if seenIn[t] == nil {
				seenIn[t] = map[string]bool{}
			}
			seenIn[t][key] = true
		}
	}
	var all []item
	for key, rs := range sessions {
		sort.Slice(rs, func(i, j int) bool { return rs[i].Offset < rs[j].Offset })
		m := byKey[key]
		sawUser := false
		for i, r := range rs {
			if r.Record.Role != "user" {
				continue
			}
			f.userTurns++
			first := !sawUser
			sawUser = true
			if first {
				f.first++
				continue
			}
			switch index.CorrectionReject(r.Record.Text) {
			case "plumbing":
				f.plumbing++
				continue
			case "short":
				f.short++
				continue
			case "long":
				f.long++
				continue
			}
			if len(seenIn[strings.TrimSpace(r.Record.Text)]) >= automatedSessions {
				f.automated++
				continue
			}
			prev, prevCmd, hasBefore := before(rs[:i])
			if !hasBefore {
				f.nothingBefore++
				continue
			}
			reply, cmds := after(rs[i+1:])
			s := index.ScoreCorrection(r.Record.Text, reply, cmds)
			all = append(all, item{
				Harness: m.Harness, Project: m.Project, Time: r.Record.Time,
				Prev: tail(prev, 300), PrevCmd: tail(prevCmd, 160), User: r.Record.Text, Reply: head(reply, 300),
				Kind: s.Kind, Marker: s.Marker, Apology: s.Apology, Norm: s.Normative, Undo: s.Undo, Anger: s.Anger,
				Score: s.Score, Accepted: s.Accepted,
			})
		}
	}
	if len(all) == 0 {
		return dir, nil, f, fmt.Errorf("no candidate turns in %s — is the index built (`deja index`)?", dir)
	}
	return dir, all, f, nil
}

func sample(out string, n, top int, seed int64) error {
	warnIfInRepo(out)
	dir, all, _, err := collect()
	if err != nil {
		return err
	}
	var acc, rej []item
	for _, it := range all {
		if it.Accepted {
			acc = append(acc, it)
		} else {
			rej = append(rej, it)
		}
	}
	sort.SliceStable(acc, func(i, j int) bool {
		if acc[i].Score != acc[j].Score {
			return acc[i].Score > acc[j].Score
		}
		return acc[i].Time.After(acc[j].Time)
	})
	if top > len(acc) {
		top = len(acc)
	}
	rng := rand.New(rand.NewSource(seed))
	rest := n - top
	nAcc := rest / 2
	nRej := rest - nAcc
	accRest := acc[top:]
	if nAcc > len(accRest) {
		nRej += nAcc - len(accRest)
		nAcc = len(accRest)
	}
	if nRej > len(rej) {
		nRej = len(rej)
	}
	var picked []item
	for _, it := range acc[:top] {
		it.Stratum = stratumTop
		picked = append(picked, it)
	}
	for _, it := range draw(rng, accRest, nAcc) {
		it.Stratum = stratumAccepted
		picked = append(picked, it)
	}
	for _, it := range draw(rng, rej, nRej) {
		it.Stratum = stratumRejected
		picked = append(picked, it)
	}
	// Labelling in stratum order would tell the labeller the detector's
	// verdict; shuffle so every turn is judged on its own text.
	rng.Shuffle(len(picked), func(i, j int) { picked[i], picked[j] = picked[j], picked[i] })
	for i := range picked {
		picked[i].ID = i + 1
	}
	md := meta{Index: dir, SampledAt: time.Now(), Candidates: len(all), Seed: seed,
		Population: map[string]int{stratumTop: top, stratumAccepted: len(accRest), stratumRejected: len(rej)}}
	if err := write(out, md, picked); err != nil {
		return err
	}
	fmt.Printf("%d candidate turns · %d accepted, %d rejected\n", len(all), len(acc), len(rej))
	fmt.Printf("sampled %d (top %d · accepted %d · rejected %d) → %s\n", len(picked), top, nAcc, nRej, out)
	fmt.Println("next: correctionsprobe label")
	return nil
}

// before returns the nearest agent reply and command before a turn, and
// whether anything at all came before it to correct.
func before(rs []index.OffsetRecord) (reply, cmd string, ok bool) {
	for i := len(rs) - 1; i >= 0; i-- {
		r := rs[i].Record
		if r.Role == "user" {
			break
		}
		ok = true
		if r.Role == "assistant" && reply == "" {
			reply = r.Text
		}
		if r.Role == "command" && cmd == "" {
			cmd = r.Text
		}
	}
	return reply, cmd, ok
}

// after returns the agent's first reply and every command up to the next
// user turn.
func after(rs []index.OffsetRecord) (reply string, cmds []string) {
	for _, r := range rs {
		switch r.Record.Role {
		case "user":
			return reply, cmds
		case "assistant":
			if reply == "" {
				reply = r.Record.Text
			}
		case "command":
			cmds = append(cmds, r.Record.Text)
		}
	}
	return reply, cmds
}

func draw(rng *rand.Rand, from []item, k int) []item {
	idx := rng.Perm(len(from))
	out := make([]item, 0, k)
	for _, i := range idx[:k] {
		out = append(out, from[i])
	}
	return out
}

func label(out string) error {
	md, items, err := read(out)
	if err != nil {
		return err
	}
	in := bufio.NewReader(os.Stdin)
	fmt.Println("d = correction with domain knowledge · w = correction about how the agent works")
	fmt.Println("n = not a correction · u = unsure · s = skip · b = back · q = quit (progress is saved)")
	for i := 0; i < len(items); {
		it := &items[i]
		if it.Label != "" {
			i++
			continue
		}
		done := 0
		for _, x := range items {
			if x.Label != "" {
				done++
			}
		}
		fmt.Printf("\n──── %d/%d · %s · %s · %s\n", done+1, len(items), it.Harness, it.Project, it.Time.Local().Format("2006-01-02"))
		if it.Prev != "" {
			fmt.Printf("AGENT  %s\n", oneLine(it.Prev))
		}
		if it.PrevCmd != "" {
			fmt.Printf("RAN    %s\n", oneLine(it.PrevCmd))
		}
		fmt.Printf("YOU ▶  %s\n", it.User)
		if it.Reply != "" {
			fmt.Printf("AGENT  %s\n", oneLine(it.Reply))
		}
		fmt.Print("[d/w/n/u/s/b/q] > ")
		line, err := in.ReadString('\n')
		key := strings.TrimSpace(strings.ToLower(line))
		if err != nil {
			key = "q"
		}
		switch key {
		case "d", "w", "n", "u":
			it.Label = key
			if err := write(out, md, items); err != nil {
				return err
			}
			i++
		case "s":
			i++
		case "b":
			for j := i - 1; j >= 0; j-- {
				if items[j].Label != "" {
					items[j].Label = ""
					i = j
					break
				}
			}
		case "q":
			return write(out, md, items)
		}
	}
	fmt.Println("\nall labelled. next: correctionsprobe eval")
	return write(out, md, items)
}

// eval prints only counts and rates, and marker strings from the detector's
// own tables — nothing from the corpus unless features is set.
func eval(out string, features bool) error {
	md, items, err := read(out)
	if err != nil {
		return err
	}
	type tally struct{ yes, no int }
	per := map[string]*tally{}
	for _, s := range []string{stratumTop, stratumAccepted, stratumRejected} {
		per[s] = &tally{}
	}
	var dom, work, unsure, open int
	for _, it := range items {
		switch it.Label {
		case "d", "w":
			per[it.Stratum].yes++
			if it.Label == "d" {
				dom++
			} else {
				work++
			}
		case "n":
			per[it.Stratum].no++
		case "u":
			unsure++
		default:
			open++
		}
	}
	rate := func(t *tally) float64 {
		if t.yes+t.no == 0 {
			return math.NaN()
		}
		return float64(t.yes) / float64(t.yes+t.no)
	}
	fmt.Printf("index %s · sampled %s · %d candidates\n", md.Index, md.SampledAt.Local().Format("2006-01-02"), md.Candidates)
	fmt.Printf("labelled %d · unsure %d · unlabelled %d\n\n", len(items)-unsure-open, unsure, open)
	fmt.Println("== by stratum")
	for _, s := range []string{stratumTop, stratumAccepted, stratumRejected} {
		t := per[s]
		fmt.Printf("  %-9s population %5d · labelled %3d · corrections %s\n", s, md.Population[s], t.yes+t.no, pct(rate(t)))
	}
	pTop, pAcc, pRej := rate(per[stratumTop]), rate(per[stratumAccepted]), rate(per[stratumRejected])
	tp := nz(pTop)*float64(md.Population[stratumTop]) + nz(pAcc)*float64(md.Population[stratumAccepted])
	fn := nz(pRej) * float64(md.Population[stratumRejected])
	accepted := md.Population[stratumTop] + md.Population[stratumAccepted]
	fmt.Println("\n== estimates over the whole index")
	fmt.Printf("  precision@%d           %s   (target ≥ 80%%)\n", md.Population[stratumTop], pct(pTop))
	if accepted > 0 {
		fmt.Printf("  precision, accepted    %5.1f%%   (~%.0f of %d)\n", 100*tp/float64(accepted), tp, accepted)
	}
	if tp+fn > 0 {
		fmt.Printf("  recall                 %5.1f%%   (~%.0f corrections missed)\n", 100*tp/(tp+fn), fn)
	}
	fmt.Printf("  kind                   domain %d · workflow %d\n", dom, work)

	// The labels outlive the detector that drew the sample: every labelled
	// turn still stands for population/labelled turns of its stratum, so a
	// revised detector can be scored on the same labels without labelling
	// again. Tuning on one sample overfits it; confirm on a fresh seed.
	sw := map[string]float64{}
	for st, t := range per {
		if n := t.yes + t.no; n > 0 {
			sw[st] = float64(md.Population[st]) / float64(n)
		}
	}
	verdict := func(name string, accept func(item) bool) {
		var tp, fp, fn float64
		for _, it := range items {
			yes, no := it.Label == "d" || it.Label == "w", it.Label == "n"
			if !yes && !no {
				continue
			}
			w := sw[it.Stratum]
			switch {
			case accept(it) && yes:
				tp += w
			case accept(it) && no:
				fp += w
			case !accept(it) && yes:
				fn += w
			}
		}
		prec, rec := tp/math.Max(tp+fp, 1e-9), tp/math.Max(tp+fn, 1e-9)
		f1 := 2 * prec * rec / math.Max(prec+rec, 1e-9)
		fmt.Printf("  %-18s accepts ~%5.0f · precision %5.1f%% · recall %5.1f%% · F1 %.2f\n", name, tp+fp, 100*prec, 100*rec, f1)
	}
	fmt.Println("\n== detector on these labels (weighted to the index)")
	verdict("as sampled", func(it item) bool { return it.Accepted })
	verdict("current code", func(it item) bool { return rescore(it).Accepted })

	// Each labelled turn stands for population/labelled turns of its stratum,
	// so signal rates describe the index rather than the sample's mix.
	weight := map[string]float64{}
	for s, t := range per {
		if n := t.yes + t.no; n > 0 {
			weight[s] = float64(md.Population[s]) / float64(n)
		}
	}
	type sig struct {
		name string
		has  func(item) bool
	}
	sigs := []sig{
		{"marker", func(it item) bool { return it.Marker != "" }},
		{"apology", func(it item) bool { return it.Apology != "" }},
		{"normative", func(it item) bool { return it.Norm }},
		{"undo", func(it item) bool { return it.Undo }},
		{"anger", func(it item) bool { return it.Anger > 0 }},
	}
	var totYes, totNo float64
	for _, it := range items {
		if it.Label == "d" || it.Label == "w" {
			totYes += weight[it.Stratum]
		} else if it.Label == "n" {
			totNo += weight[it.Stratum]
		}
	}
	fmt.Println("\n== signals (weighted to the index)")
	fmt.Println("  signal      labelled  corrections  log-likelihood ratio  current weight")
	current := map[string]int{"apology": index.CorrectionWeightApology,
		"normative": index.CorrectionWeightNormative, "undo": index.CorrectionWeightUndo}
	for _, s := range sigs {
		var yes, no float64
		var n int
		for _, it := range items {
			if !s.has(it) {
				continue
			}
			switch it.Label {
			case "d", "w":
				yes += weight[it.Stratum]
				n++
			case "n":
				no += weight[it.Stratum]
				n++
			}
		}
		llr := math.Log((yes + 0.5) / (totYes + 1) / ((no + 0.5) / (totNo + 1)))
		cw := "—"
		if w, ok := current[s.name]; ok {
			cw = fmt.Sprint(w)
		} else if s.name == "marker" {
			cw = "by kind"
		}
		fmt.Printf("  %-10s  %8d  %10.1f%%  %20.2f  %14s\n", s.name, n, 100*yes/math.Max(yes+no, 1e-9), llr, cw)
	}
	fmt.Println("  (a positive ratio means the signal points at a correction; compare its size across signals)")

	byKind := map[string]*tally{}
	for _, it := range items {
		k := it.Kind
		if k == "" {
			k = "(none)"
		}
		t := byKind[k]
		if t == nil {
			t = &tally{}
			byKind[k] = t
		}
		switch it.Label {
		case "d", "w":
			t.yes++
		case "n":
			t.no++
		}
	}
	fmt.Println("\n== marker kinds (unweighted)")
	for _, k := range []string{index.CorrectionNegation, index.CorrectionRepeat, index.CorrectionUnasked,
		index.CorrectionNotFixed, index.CorrectionUndo, index.CorrectionWhy, "(none)"} {
		t := byKind[k]
		if t == nil || t.yes+t.no == 0 {
			continue
		}
		fmt.Printf("  %-10s  weight %d  %3d labelled · %s corrections\n", k, index.CorrectionKindWeight[k], t.yes+t.no, pct(rate(t)))
	}

	byMarker := map[string]*tally{}
	for _, it := range items {
		for _, m := range []string{it.Kind + ":" + it.Marker, "apology:" + it.Apology} {
			if m == ":" || m == "apology:" {
				continue
			}
			t := byMarker[m]
			if t == nil {
				t = &tally{}
				byMarker[m] = t
			}
			switch it.Label {
			case "d", "w":
				t.yes++
			case "n":
				t.no++
			}
		}
	}
	keys := make([]string, 0, len(byMarker))
	for k := range byMarker {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := byMarker[keys[i]], byMarker[keys[j]]
		if a.yes+a.no != b.yes+b.no {
			return a.yes+a.no > b.yes+b.no
		}
		return keys[i] < keys[j]
	})
	fmt.Println("\n== markers (unweighted; prune the ones that are mostly not corrections)")
	for _, k := range keys {
		t := byMarker[k]
		if t.yes+t.no == 0 {
			continue
		}
		fmt.Printf("  %-24q  %3d labelled · %5.1f%% corrections\n", k, t.yes+t.no, 100*rate(t))
	}
	if features {
		printFeatures(items)
	}
	return nil
}

// rescore runs the current detector over a labelled turn. The commands that
// followed it were not kept, only whether one put work back.
func rescore(it item) index.CorrectionSignals {
	var cmds []string
	if it.Undo {
		cmds = []string{"git restore"}
	}
	return index.ScoreCorrection(it.User, it.Reply, cmds)
}

// printFeatures lists what separates the turns labelled corrections from the
// rest: how the agent's reply opened, and short fragments of the person's
// turn. Fragments are two or three characters — enough to find a marker the
// tables miss, too short to carry what the turn was about.
func printFeatures(items []item) {
	type pair struct{ yes, no int }
	var nYes, nNo int
	replies := map[string]*pair{}
	grams := map[string]*pair{}
	bump := func(m map[string]*pair, k string, yes bool) {
		p := m[k]
		if p == nil {
			p = &pair{}
			m[k] = p
		}
		if yes {
			p.yes++
		} else {
			p.no++
		}
	}
	for _, it := range items {
		yes, no := it.Label == "d" || it.Label == "w", it.Label == "n"
		if !yes && !no {
			continue
		}
		if yes {
			nYes++
		} else {
			nNo++
		}
		if o := opening(it.Reply); o != "" {
			bump(replies, o, yes)
		}
		r := []rune(strings.ToLower(strings.TrimSpace(it.User)))
		if len(r) > 80 {
			r = r[:80]
		}
		seen := map[string]bool{}
		for size := 2; size <= 3; size++ {
			for i := 0; i+size <= len(r); i++ {
				g := string(r[i : i+size])
				if strings.ContainsAny(g, " \t\n、。，．,.") || seen[g] {
					continue
				}
				seen[g] = true
				bump(grams, g, yes)
			}
		}
	}
	logOdds := func(p *pair) float64 {
		return math.Log((float64(p.yes) + 0.5) / float64(nYes+1) / ((float64(p.no) + 0.5) / float64(nNo+1)))
	}
	show := func(title string, m map[string]*pair, minSupport, limit int, positive bool) {
		var keys []string
		for k, p := range m {
			if p.yes+p.no >= minSupport && (logOdds(p) > 0) == positive {
				keys = append(keys, k)
			}
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := logOdds(m[keys[i]]), logOdds(m[keys[j]])
			if a != b {
				return (a > b) == positive
			}
			return keys[i] < keys[j]
		})
		if len(keys) > limit {
			keys = keys[:limit]
		}
		fmt.Printf("\n== %s\n  corrections  others  log-odds  text\n", title)
		for _, k := range keys {
			p := m[k]
			fmt.Printf("  %11d  %6d  %8.2f  %q\n", p.yes, p.no, logOdds(p), k)
		}
	}
	fmt.Printf("\n(features over %d corrections and %d others, unweighted)\n", nYes, nNo)
	show("agent reply openings that lean towards corrections", replies, 3, 25, true)
	show("agent reply openings that lean away", replies, 3, 10, false)
	show("fragments of your turn that lean towards corrections (first 80 characters)", grams, 6, 40, true)
	show("fragments that lean away", grams, 6, 15, false)
}

func pct(f float64) string {
	if math.IsNaN(f) {
		return "    —"
	}
	return fmt.Sprintf("%5.1f%%", 100*f)
}

func nz(f float64) float64 {
	if math.IsNaN(f) {
		return 0
	}
	return f
}

func write(path string, md meta, items []item) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(map[string]meta{"meta": md}); err != nil {
		f.Close()
		return err
	}
	for _, it := range items {
		if err := enc.Encode(it); err != nil {
			f.Close()
			return err
		}
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func read(path string) (meta, []item, error) {
	f, err := os.Open(path)
	if err != nil {
		return meta{}, nil, fmt.Errorf("%w — run `correctionsprobe sample` first", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	var md meta
	var items []item
	for sc.Scan() {
		line := sc.Bytes()
		if md.Index == "" {
			var h map[string]meta
			if json.Unmarshal(line, &h) == nil {
				if m, ok := h["meta"]; ok && m.Index != "" {
					md = m
					continue
				}
			}
		}
		var it item
		if err := json.Unmarshal(line, &it); err != nil {
			return md, nil, fmt.Errorf("%s: %w", path, err)
		}
		items = append(items, it)
	}
	return md, items, sc.Err()
}

func warnIfInRepo(path string) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return
	}
	for d := filepath.Dir(abs); ; d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			fmt.Fprintf(os.Stderr, "warning: %s is inside the repository at %s — this file holds private history; write it elsewhere with -o\n", path, d)
			return
		}
		if filepath.Dir(d) == d {
			return
		}
	}
}

func head(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func tail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// stats describes the whole candidate set without sampling or printing any
// of the person's own text, so the detector can be tuned before anyone spends
// an hour labelling a sample it would have drawn badly.
func stats(openings bool) error {
	dir, all, f, err := collect()
	if err != nil {
		return err
	}
	fmt.Printf("index %s\n\n== funnel\n", dir)
	fmt.Printf("  user turns              %7d\n", f.userTurns)
	fmt.Printf("  - first in session      %7d\n", f.first)
	fmt.Printf("  - plumbing              %7d\n", f.plumbing)
	fmt.Printf("  - too short             %7d\n", f.short)
	fmt.Printf("  - too long              %7d\n", f.long)
	fmt.Printf("  - same text in %d+ sess. %7d\n", automatedSessions, f.automated)
	fmt.Printf("  - nothing before        %7d\n", f.nothingBefore)
	fmt.Printf("  = candidates            %7d\n", len(all))

	count := func(keep func(item) bool) int {
		n := 0
		for _, it := range all {
			if keep(it) {
				n++
			}
		}
		return n
	}
	share := func(n int) string { return fmt.Sprintf("%7d  %5.1f%%", n, 100*float64(n)/float64(len(all))) }
	fmt.Println("\n== signals over candidates")
	fmt.Printf("  marker                  %s\n", share(count(func(it item) bool { return it.Marker != "" })))
	fmt.Printf("  apology                 %s\n", share(count(func(it item) bool { return it.Apology != "" })))
	fmt.Printf("  normative               %s\n", share(count(func(it item) bool { return it.Norm })))
	fmt.Printf("  undo                    %s\n", share(count(func(it item) bool { return it.Undo })))
	fmt.Printf("  anger                   %s\n", share(count(func(it item) bool { return it.Anger > 0 })))
	fmt.Printf("  no reply after the turn %s\n", share(count(func(it item) bool { return it.Reply == "" })))
	fmt.Println("\n== combinations")
	fmt.Printf("  marker + apology        %s\n", share(count(func(it item) bool { return it.Marker != "" && it.Apology != "" })))
	fmt.Printf("  marker, no apology      %s\n", share(count(func(it item) bool { return it.Marker != "" && it.Apology == "" })))
	fmt.Printf("    of which normative    %s\n", share(count(func(it item) bool { return it.Marker != "" && it.Apology == "" && it.Norm })))
	fmt.Printf("  apology, no marker      %s\n", share(count(func(it item) bool { return it.Marker == "" && it.Apology != "" })))
	fmt.Printf("  accepted                %s\n", share(count(func(it item) bool { return it.Accepted })))

	scores := map[int]int{}
	for _, it := range all {
		scores[it.Score]++
	}
	fmt.Println("\n== score")
	for sc := 0; sc <= 7; sc++ {
		if scores[sc] > 0 {
			fmt.Printf("  %d  %7d\n", sc, scores[sc])
		}
	}

	type tally struct{ cand, marker, apology, accepted int }
	byHarness := map[string]*tally{}
	byMarker := map[string]int{}
	for _, it := range all {
		t := byHarness[it.Harness]
		if t == nil {
			t = &tally{}
			byHarness[it.Harness] = t
		}
		t.cand++
		if it.Marker != "" {
			t.marker++
			byMarker[it.Kind+":"+it.Marker]++
		}
		if it.Apology != "" {
			t.apology++
			byMarker["apology:"+it.Apology]++
		}
		if it.Accepted {
			t.accepted++
		}
	}
	fmt.Println("\n== by harness          candidates   marker  apology  accepted")
	for _, h := range sortedByCount(func(yield func(string, int)) {
		for k, t := range byHarness {
			yield(k, t.cand)
		}
	}) {
		t := byHarness[h]
		fmt.Printf("  %-20s %9d %8d %8d %9d\n", h, t.cand, t.marker, t.apology, t.accepted)
	}
	fmt.Println("\n== markers that fired (from the detector's own tables)")
	for _, k := range sortedByCount(func(yield func(string, int)) {
		for k, n := range byMarker {
			yield(k, n)
		}
	}) {
		fmt.Printf("  %-26q %6d\n", k, byMarker[k])
	}

	if openings {
		// How agents answer a turn that opened with a correction marker,
		// against how they answer everything else. An opening that is much
		// more common after a marker is an acknowledgement phrase the apology
		// table is missing.
		after, base := map[string]int{}, map[string]int{}
		var nAfter, nBase int
		for _, it := range all {
			o := opening(it.Reply)
			if o == "" {
				continue
			}
			if it.Marker != "" {
				after[o]++
				nAfter++
			} else {
				base[o]++
				nBase++
			}
		}
		fmt.Println("\n== reply openings after a marker (agent text; lift = how much more common than elsewhere)")
		fmt.Println("  after  elsewhere   lift  opening")
		for i, k := range sortedByCount(func(yield func(string, int)) {
			for k, n := range after {
				yield(k, n)
			}
		}) {
			if i == 40 || after[k] < 3 {
				break
			}
			lift := (float64(after[k]) / float64(nAfter)) / ((float64(base[k]) + 0.5) / float64(nBase+1))
			fmt.Printf("  %5d  %9d  %5.1f  %s\n", after[k], base[k], lift, k)
		}
	}
	return nil
}

// opening is the first few words of a reply, folded so the same
// acknowledgement compares equal whatever follows it.
func opening(reply string) string {
	r := []rune(strings.TrimSpace(reply))
	for i, c := range r {
		if c == '\n' || c == '。' || c == '.' || c == '!' || c == '！' || c == ',' || c == '、' {
			r = r[:i]
			break
		}
	}
	if len(r) > 16 {
		r = r[:16]
	}
	return strings.ToLower(strings.TrimSpace(string(r)))
}

func sortedByCount(each func(func(string, int))) []string {
	counts := map[string]int{}
	each(func(k string, n int) { counts[k] = n })
	keys := make([]string, 0, len(counts))
	for k := range counts {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if counts[keys[i]] != counts[keys[j]] {
			return counts[keys[i]] > counts[keys[j]]
		}
		return keys[i] < keys[j]
	})
	return keys
}
