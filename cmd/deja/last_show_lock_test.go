package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// `deja last` and `deja show` only read what is indexed. With another deja
// holding the index lock for its whole pass (a `deja index`, a hook refresh)
// they waited that pass out, and a scheduled `deja last` sat at no CPU for over
// an hour. They answer from the index on disk instead, hand the refresh to the
// warmup and say so once; with the lock free they still bring the index up to
// date first.
func TestLastAndShowAnswerWhileAnotherDejaHoldsTheIndexLock(t *testing.T) {
	dir := seedRecallPage(t)
	// A transcript the index has not read yet: with the lock free, this is
	// what makes Ensure run a pass before answering.
	line := `{"type":"user","message":{"role":"user","content":"the newest session"},` +
		`"timestamp":"2026-09-01T10:00:00Z","sessionId":"sessz","cwd":"/proj"}` + "\n"
	store := filepath.Join(os.Getenv("DEJA_CLAUDE_ROOT"), "-proj")
	if err := os.WriteFile(filepath.Join(store, "sessz.jsonl"), []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	// The suite's guard keeps requestWarmup from getting as far as a spawn;
	// lift it and count the spawns instead.
	t.Setenv("DEJA_WARMUP_SENTINEL", "")
	spawned := 0
	oldSpawn := spawnWarmup
	spawnWarmup = func(string, string) error { spawned++; return nil }
	t.Cleanup(func() { spawnWarmup = oldSpawn })

	unlock := holdIndexLock(t, dir)
	saidReadingAsItWas = sync.Once{}
	runs := []struct {
		name string
		run  func() error
		want string
	}{
		{"last --json --harness", func() error { return cmdLast(dir, []string{"3", "--json", "--harness", "claude"}, "") }, `"sessi"`},
		{"show --harness --json", func() error { return cmdShow(dir, []string{"sessa", "--harness", "claude", "--json"}, "") }, `"sessa"`},
		{"show <prefix>", func() error { return cmdShow(dir, []string{"sessb"}, "") }, "sessb"},
	}
	var waited string
	stdout := make([]string, len(runs))
	stderr := captureStderr(t, func() {
		for i, r := range runs {
			stdout[i] = captureStdout(t, func() {
				done := make(chan error, 1)
				go func() { done <- r.run() }()
				select {
				case err := <-done:
					if err != nil {
						t.Errorf("%s: %v", r.name, err)
					}
				case <-time.After(5 * time.Second):
					waited = r.name
				}
			})
			if waited != "" {
				return
			}
		}
	})
	if waited != "" {
		unlock()
		t.Fatalf("%s waited for the index lock another deja holds", waited)
	}
	for i, r := range runs {
		if !strings.Contains(stdout[i], r.want) {
			t.Errorf("%s did not answer from the snapshot:\n%s", r.name, stdout[i])
		}
		if strings.Contains(stdout[i], "sessz") {
			t.Errorf("%s updated the index under a held lock instead of reading it as it was:\n%s", r.name, stdout[i])
		}
	}
	if n := strings.Count(stderr, "answering from the index as it was"); n != 1 {
		t.Errorf("the stale-answer line went out %d times, want once:\n%s", n, stderr)
	}
	if spawned != 1 {
		t.Errorf("the warmup was asked for %d times, want once (its sentinel holds the rest)", spawned)
	}
	unlock()

	saidReadingAsItWas = sync.Once{}
	var free string
	stderr = captureStderr(t, func() {
		free = captureStdout(t, func() {
			if err := cmdLast(dir, []string{"1", "--json"}, ""); err != nil {
				t.Errorf("last with the lock free: %v", err)
			}
		})
	})
	if !strings.Contains(free, "sessz") {
		t.Errorf("with the lock free, last did not index the new transcript first:\n%s", free)
	}
	if strings.Contains(stderr, "as it was") {
		t.Errorf("with the lock free, last said it answered from a stale index:\n%s", stderr)
	}
}
