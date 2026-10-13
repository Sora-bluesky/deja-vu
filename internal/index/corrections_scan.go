package index

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// CorrectionTurn is one user turn with the turn before it and the reply after
// it — the three things a person, or an agent, needs to judge whether it
// corrected something and what.
type CorrectionTurn struct {
	Session SessionMeta
	Time    time.Time
	// Prev is the end of the agent's last reply before the turn, PrevCmd the
	// last command it ran; Reply is the start of its next reply. Each is cut
	// to correctionContextRunes, which is what the labelled runs were judged on.
	Prev, PrevCmd, User, Reply string
	Signals                    CorrectionSignals
}

// CorrectionFunnel counts where user turns dropped out before scoring, so an
// empty answer can say which filter emptied it.
type CorrectionFunnel struct {
	UserTurns, First, Plumbing, Short, Long, Automated, NothingBefore int
}

const (
	correctionContextRunes = 300
	correctionCommandRunes = 160
	// CorrectionAutomatedSessions is how many separate sessions the same user
	// text must appear in before it is taken for a scripted prompt rather
	// than a person: the first real run had one instruction, sent verbatim by
	// a loop, as its most frequent "correction".
	CorrectionAutomatedSessions = 5
)

// skeletonRecord is what the first pass keeps of every record: enough to put
// a session's turns in order and find a turn's neighbours, without holding
// the text of three hundred thousand messages.
type skeletonRecord struct {
	off  int64
	role string
	text string // user turns that can be candidates only
	at   time.Time
}

// ScanCorrections scores every user turn in the store that has something
// before it to correct. It reads the record log twice: once for the order of
// every session and the text of user turns, once for the few agent replies
// and commands either side of a candidate.
func ScanCorrections(dir string) ([]CorrectionTurn, CorrectionFunnel, error) {
	var f CorrectionFunnel
	if dir == "" {
		dir = DefaultDir()
	}
	m, err := readManifestCached(dir)
	if err != nil {
		return nil, f, err
	}
	t := tablesFromManifest(m)
	path := filepath.Join(dir, "records.bin")
	sessions := map[string][]skeletonRecord{}
	seenIn := map[string]map[string]bool{}
	err = eachRecordWithOffset(path, t, func(off int64, r Record) {
		switch r.Role {
		case "user":
			sk := skeletonRecord{off: off, role: r.Role, at: r.Time}
			if CorrectionCandidate(r.Text) {
				sk.text = strings.TrimSpace(r.Text)
				if seenIn[sk.text] == nil {
					seenIn[sk.text] = map[string]bool{}
				}
				seenIn[sk.text][r.Key] = true
			} else {
				sk.text = correctionRejectTag + CorrectionReject(r.Text)
			}
			sessions[r.Key] = append(sessions[r.Key], sk)
		case "assistant", roleToolOutput, roleCommand:
			sessions[r.Key] = append(sessions[r.Key], skeletonRecord{off: off, role: r.Role, at: r.Time})
		}
	})
	if err != nil {
		return nil, f, err
	}

	type pending struct {
		key                     string
		user                    skeletonRecord
		prev, prevCmd, replyOff int64
	}
	var cands []pending
	want := map[int64]bool{}
	for key, rs := range sessions {
		// A session is written in message order, and an incremental update
		// rewrites a changed session whole, so offset order within a key is
		// turn order. Sessions written concurrently interleave; grouping by
		// key undoes it.
		sort.Slice(rs, func(i, j int) bool { return rs[i].off < rs[j].off })
		sawUser := false
		for i, r := range rs {
			if r.role != "user" {
				continue
			}
			f.UserTurns++
			if !sawUser {
				sawUser = true
				f.First++
				continue
			}
			if reason, rejected := strings.CutPrefix(r.text, correctionRejectTag); rejected {
				switch reason {
				case "plumbing":
					f.Plumbing++
				case "short":
					f.Short++
				default:
					f.Long++
				}
				continue
			}
			if len(seenIn[r.text]) >= CorrectionAutomatedSessions {
				f.Automated++
				continue
			}
			p := pending{key: key, user: r, prev: -1, prevCmd: -1, replyOff: -1}
			hasBefore := false
			for j := i - 1; j >= 0 && rs[j].role != "user"; j-- {
				hasBefore = true
				if rs[j].role == "assistant" && p.prev < 0 {
					p.prev = rs[j].off
				}
				if rs[j].role == roleCommand && p.prevCmd < 0 {
					p.prevCmd = rs[j].off
				}
			}
			if !hasBefore {
				f.NothingBefore++
				continue
			}
			for j := i + 1; j < len(rs) && rs[j].role != "user"; j++ {
				if rs[j].role == "assistant" {
					p.replyOff = rs[j].off
					break
				}
			}
			for _, o := range []int64{p.prev, p.prevCmd, p.replyOff} {
				if o >= 0 {
					want[o] = true
				}
			}
			cands = append(cands, p)
		}
	}

	texts := make(map[int64]string, len(want))
	offs := make([]int64, 0, len(want))
	for o := range want {
		offs = append(offs, o)
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	file, err := os.Open(path)
	if err != nil {
		return nil, f, err
	}
	defer func() { _ = file.Close() }()
	// A few thousand reads at known offsets, in file order. Reading each one
	// on its own keeps the offset beside the text; a record that will not
	// decode leaves its slot empty rather than shifting the rest.
	for _, o := range offs {
		if r, err := readRecordAt(file, o, t); err == nil {
			texts[o] = r.Text
		}
	}

	out := make([]CorrectionTurn, 0, len(cands))
	for _, p := range cands {
		ct := CorrectionTurn{Session: m.Sessions[p.key], Time: p.user.at, User: p.user.text}
		if p.prev >= 0 {
			ct.Prev = tailRunes(texts[p.prev], correctionContextRunes)
		}
		if p.prevCmd >= 0 {
			ct.PrevCmd = tailRunes(texts[p.prevCmd], correctionCommandRunes)
		}
		if p.replyOff >= 0 {
			ct.Reply = headRunes(texts[p.replyOff], correctionContextRunes)
		}
		ct.Signals = ScoreCorrection(ct.User, ct.Reply, nil)
		out = append(out, ct)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, f, nil
}

// correctionRejectTag marks a user turn the first pass already ruled out, so
// the second can count why without keeping its text.
const correctionRejectTag = "\x00reject:"

func tailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[len(r)-n:])
	}
	return s
}

// eachRecordWithOffset is eachRecord that also reports where each record
// starts, counted from the lengths it reads rather than by seeking — a seek
// per record is what made ReadRecords slow on a large log.
func eachRecordWithOffset(path string, t *recordTables, fn func(int64, Record)) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	r := bufio.NewReaderSize(f, 1024*1024)
	var off int64
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(r, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		n := binary.LittleEndian.Uint32(hdr[:])
		if n > maxRecordSize {
			return fmt.Errorf("%w: record length %d exceeds cap", errCorruptIndex, n)
		}
		b := make([]byte, n)
		if _, err := io.ReadFull(r, b); err != nil {
			if err == io.ErrUnexpectedEOF {
				return nil
			}
			return err
		}
		rec, err := decodeRecord(b, t)
		if err != nil {
			return err
		}
		fn(off, rec)
		off += 4 + int64(n)
	}
}
