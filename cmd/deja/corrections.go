package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/vshulcz/deja-vu/internal/index"
	"github.com/vshulcz/deja-vu/internal/policy"
)

// `deja corrections` lists the turns where a person corrected an agent.
//
// Those turns are where a person's own knowledge gets written down without
// them meaning to: the agent assumed one thing, and they wrote out what is
// actually true here. docs/design/corrections.md has the measurements this
// rests on. The short version: the person's wording barely predicts a
// correction — people correct in their own words — but the agent's reply
// does, because agents concede in a few stock phrases. On two labelled
// samples that acknowledgement held at 82-90% precision and found 10-18% of
// corrections.
//
// So the command has two layers. The default listing is the acknowledged
// corrections: a small, mostly-right list deja can produce on its own.
// --candidates hands every turn that could be a correction, with the turn
// before and the reply after, to whoever can read meaning — the person, or
// their agent through the /deja-corrections command — which is how the other
// four fifths are found. Neither layer writes anything: what is worth keeping
// goes in through `deja remember`, by a person's decision.

type correctionsOptions struct {
	project, harness string
	since            time.Duration
	limit, offset    int
	json, candidates bool
}

func parseCorrections(args []string) (correctionsOptions, error) {
	o := correctionsOptions{limit: 20}
	limitSet := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--json":
			o.json = true
		case "--candidates":
			o.candidates = true
		case "--project", "--harness", "--since", "--limit", "--offset":
			if i+1 >= len(args) {
				return o, fmt.Errorf("corrections: %s needs a value", a)
			}
			i++
			v := args[i]
			switch a {
			case "--project":
				o.project = v
			case "--harness":
				o.harness = v
			case "--since":
				d, err := parseDur(v)
				if err != nil {
					return o, err
				}
				o.since = d
			case "--limit", "--offset":
				n, err := strconv.Atoi(v)
				if err != nil || n < 0 || (a == "--limit" && n == 0) {
					return o, fmt.Errorf("corrections: %s wants a positive number, got %q", a, v)
				}
				if a == "--limit" {
					o.limit = n
					limitSet = true
				} else {
					o.offset = n
				}
			}
		default:
			return o, fmt.Errorf("corrections: unknown argument %q — see `deja help`", a)
		}
	}
	// A page of candidates is what an agent reads in one go; the listing is
	// what fits on a screen.
	if o.candidates && !limitSet {
		o.limit = 50
	}
	return o, nil
}

func runCorrections(dir string, args []string, stdout io.Writer) error {
	o, err := parseCorrections(args)
	if err != nil {
		return err
	}
	if err := index.Ensure(dir, "", false, os.Stderr); err != nil {
		return ensureError(dir, err)
	}
	turns, funnel, err := index.ScanCorrections(dir)
	if err != nil {
		return fmt.Errorf("corrections: %w", err)
	}
	pol := policy.Load()
	cutoff := time.Time{}
	if o.since > 0 {
		cutoff = time.Now().Add(-o.since)
	}
	var kept []index.CorrectionTurn
	hidden := 0
	for _, t := range turns {
		if o.project != "" && !strings.Contains(strings.ToLower(t.Session.Project), strings.ToLower(o.project)) {
			continue
		}
		if o.harness != "" && !strings.EqualFold(t.Session.Harness, o.harness) {
			continue
		}
		if !cutoff.IsZero() && t.Time.Before(cutoff) {
			continue
		}
		if !o.candidates && !t.Signals.Accepted {
			continue
		}
		// Every other path that shows session text runs it through the
		// trust policy; this one shows the person's own turns verbatim.
		if !pol.Allows(policy.ActivationSearch, t.Session.Project) {
			hidden++
			continue
		}
		kept = append(kept, t)
	}
	if o.candidates {
		return printCorrectionCandidates(stdout, o, kept, hidden)
	}
	return printCorrections(stdout, o, kept, len(turns), funnel, hidden)
}

// correctionGroup is acknowledged corrections that say much the same thing.
type correctionGroup struct {
	turns    []index.CorrectionTurn
	sessions map[string]bool
}

// groupCorrections puts together corrections that share at least half the
// terms of the shorter one, and at least two. Corrections are rephrased every
// time, so exact matching would never group two, and Jaccard punished the
// rephrasing that added context ("このリポは…"); measuring against the shorter
// side lets one wording contain the other.
func groupCorrections(turns []index.CorrectionTurn) []*correctionGroup {
	const threshold = 0.5
	var groups []*correctionGroup
	var shingles [][]map[string]bool
	for _, t := range turns {
		sh := index.CorrectionShingles(t.User)
		placed := false
		for gi, g := range groups {
			for _, other := range shingles[gi] {
				if overlap(sh, other) >= threshold {
					g.turns = append(g.turns, t)
					g.sessions[t.Session.Harness+":"+t.Session.ID] = true
					shingles[gi] = append(shingles[gi], sh)
					placed = true
					break
				}
			}
			if placed {
				break
			}
		}
		if !placed {
			groups = append(groups, &correctionGroup{
				turns:    []index.CorrectionTurn{t},
				sessions: map[string]bool{t.Session.Harness + ":" + t.Session.ID: true},
			})
			shingles = append(shingles, []map[string]bool{sh})
		}
	}
	// What was taught in more than one session first: that is the knowledge
	// the person keeps having to hand over. Then newest.
	sort.SliceStable(groups, func(i, j int) bool {
		if len(groups[i].sessions) != len(groups[j].sessions) {
			return len(groups[i].sessions) > len(groups[j].sessions)
		}
		return groups[i].turns[0].Time.After(groups[j].turns[0].Time)
	})
	return groups
}

func overlap(a, b map[string]bool) float64 {
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	if inter < 2 {
		return 0
	}
	return float64(inter) / float64(min(len(a), len(b)))
}

type correctionJSON struct {
	Time         time.Time `json:"time"`
	Harness      string    `json:"harness"`
	Project      string    `json:"project"`
	Session      string    `json:"session"`
	Prev         string    `json:"agent_before,omitempty"`
	PrevCmd      string    `json:"command_before,omitempty"`
	User         string    `json:"turn"`
	Reply        string    `json:"agent_after,omitempty"`
	Acknowledged bool      `json:"acknowledged"`
	Ack          string    `json:"acknowledgement,omitempty"`
	Kind         string    `json:"marker_kind,omitempty"`
	Group        int       `json:"group,omitempty"`
}

func toCorrectionJSON(t index.CorrectionTurn, group int) correctionJSON {
	return correctionJSON{
		Time: t.Time, Harness: t.Session.Harness, Project: t.Session.Project, Session: t.Session.ID,
		Prev: t.Prev, PrevCmd: t.PrevCmd, User: t.User, Reply: t.Reply,
		Acknowledged: t.Signals.Accepted, Ack: t.Signals.Apology, Kind: t.Signals.Kind, Group: group,
	}
}

func printCorrections(w io.Writer, o correctionsOptions, kept []index.CorrectionTurn, read int, f index.CorrectionFunnel, hidden int) error {
	groups := groupCorrections(kept)
	if o.json {
		out := []correctionJSON{}
		for gi, g := range groups {
			for _, t := range g.turns {
				out = append(out, toCorrectionJSON(t, gi+1))
			}
		}
		return writeCorrectionsJSON(w, out)
	}
	if len(groups) == 0 {
		fmt.Fprint(w, policyHiddenNote(policy.ActivationSearch, hidden))
		switch {
		case f.UserTurns == 0:
			fmt.Fprintln(w, "no corrections — no user turns are indexed yet")
		case read == 0:
			fmt.Fprintf(w, "no corrections — none of the %d user turns could be one (%d opened a session, %d were harness plumbing, %d too long, %d sent verbatim in %d+ sessions)\n",
				f.UserTurns, f.First, f.Plumbing, f.Long, f.Automated, index.CorrectionAutomatedSessions)
		default:
			fmt.Fprintf(w, "no acknowledged corrections in %d turns read — `deja corrections --candidates` lists the turns an agent did not concede, for you or your agent to judge\n", read)
		}
		return nil
	}
	total := len(kept)
	fmt.Fprintf(w, "what you corrected agents on — %d acknowledged in %d turn%s read\n", total, read, pluralS(read))
	shown := groups
	if len(shown) > o.limit {
		shown = shown[:o.limit]
	}
	for _, g := range shown {
		t := g.turns[0]
		n := len(g.sessions)
		fmt.Fprintf(w, "  %2d session%s  %s\n", n, pluralPad(n), trimCorrection(t.User, 88))
		if t.Prev != "" {
			fmt.Fprintf(w, "               agent had said: %s\n", trimCorrection(t.Prev, 70))
		}
		fmt.Fprintf(w, "               %s · %s · %s", t.Session.Harness, t.Session.Project, t.Time.Local().Format("Jan 2"))
		if t.Signals.Kind != "" {
			fmt.Fprintf(w, " · %s", t.Signals.Kind)
		}
		fmt.Fprintln(w)
	}
	if len(groups) > len(shown) {
		fmt.Fprintf(w, "  … %d more — --limit %d\n", len(groups)-len(shown), len(groups))
	}
	fmt.Fprint(w, policyHiddenNote(policy.ActivationSearch, hidden))
	fmt.Fprintln(w, "these are the corrections an agent conceded, about one in six of them; `--candidates` hands the rest to an agent to judge")
	fmt.Fprintln(w, "keep one: deja remember --tag correction --project <name> \"<what is actually true>\"")
	return nil
}

func printCorrectionCandidates(w io.Writer, o correctionsOptions, kept []index.CorrectionTurn, hidden int) error {
	total := len(kept)
	if o.offset > len(kept) {
		kept = nil
	} else {
		kept = kept[o.offset:]
	}
	if len(kept) > o.limit {
		kept = kept[:o.limit]
	}
	if o.json {
		out := struct {
			Total  int              `json:"total"`
			Offset int              `json:"offset"`
			Turns  []correctionJSON `json:"turns"`
		}{Total: total, Offset: o.offset, Turns: []correctionJSON{}}
		for _, t := range kept {
			out.Turns = append(out.Turns, toCorrectionJSON(t, 0))
		}
		return writeCorrectionsJSON(w, out)
	}
	if len(kept) == 0 {
		fmt.Fprintf(w, "%d candidate turn%s, newest first — none past #%d\n", total, pluralS(total), o.offset)
	} else {
		fmt.Fprintf(w, "%d candidate turn%s, newest first — showing #%d–#%d\n", total, pluralS(total), o.offset+1, o.offset+len(kept))
	}
	for i, t := range kept {
		fmt.Fprintf(w, "\n#%d · %s · %s · %s", o.offset+i+1, t.Session.Harness, t.Session.Project, t.Time.Local().Format("2006-01-02"))
		if t.Signals.Accepted {
			fmt.Fprint(w, " · acknowledged")
		}
		fmt.Fprintln(w)
		if t.Prev != "" {
			fmt.Fprintf(w, "  agent: %s\n", oneLineText(t.Prev))
		}
		fmt.Fprintf(w, "  you:   %s\n", oneLineText(t.User))
		if t.Reply != "" {
			fmt.Fprintf(w, "  agent: %s\n", oneLineText(t.Reply))
		}
	}
	fmt.Fprint(w, policyHiddenNote(policy.ActivationSearch, hidden))
	if o.offset+len(kept) < total {
		fmt.Fprintf(w, "\nnext page: --offset %d\n", o.offset+len(kept))
	}
	return nil
}

func writeCorrectionsJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func trimCorrection(s string, n int) string {
	s = oneLineText(s)
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

// pluralPad keeps the column aligned whether or not "sessions" has its s.
func pluralPad(n int) string {
	if n == 1 {
		return " "
	}
	return "s"
}
