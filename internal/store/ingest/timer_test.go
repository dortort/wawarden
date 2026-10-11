package ingest

import (
	"testing"
	"time"
)

func TestTheChatTimerIsTheNewestExpiringRowsSpan(t *testing.T) {
	s := openStore(t)
	timer := func(jid string) time.Duration {
		t.Helper()
		d, err := s.ChatTimer(t.Context(), chat(t, jid))
		if err != nil {
			t.Fatalf("ChatTimer(%s): %v", jid, err)
		}
		return d
	}
	if d := timer(alice); d != 0 {
		t.Fatalf("an empty chat has a timer of %v", d)
	}
	plain := textMessage(t, alice, "A1", alice, "plain")
	insert(t, s, plain)
	if d := timer(alice); d != 0 {
		t.Fatalf("a chat without an expiring row has a timer of %v", d)
	}
	week := textMessage(t, alice, "A2", alice, "a week")
	week.Timestamp, week.ExpiresAt = epoch.Add(time.Minute), epoch.Add(time.Minute+7*24*time.Hour)
	insert(t, s, week)
	day := textMessage(t, alice, "A3", alice, "a day")
	day.Timestamp, day.ExpiresAt = epoch.Add(-time.Hour), epoch.Add(23*time.Hour)
	insert(t, s, day)
	later := textMessage(t, alice, "A4", alice, "no timer any more")
	later.Timestamp = epoch.Add(time.Hour)
	insert(t, s, later)
	if d := timer(alice); d != 7*24*time.Hour {
		t.Fatalf("timer = %v, want the newest expiring row's week", d)
	}
	odd := textMessage(t, bob, "B1", bob, "odd span")
	odd.ExpiresAt = epoch.Add(1500 * time.Millisecond)
	insert(t, s, odd)
	if d := timer(bob); d != 2*time.Second {
		t.Fatalf("timer = %v, want the span rounded up to whole seconds", d)
	}
	if d := timer(carol); d != 0 {
		t.Fatalf("another chat's rows gave a timer of %v", d)
	}
}

func TestTheChatTimerFollowsTheLIDMap(t *testing.T) {
	s := openStore(t)
	write(t, s, func(tx *Tx) error {
		_, err := tx.LearnLID(chat(t, aliceLID), chat(t, alice), MappingSelf, epoch)
		return err
	})
	m := textMessage(t, aliceLID, "L1", aliceLID, "under the LID")
	m.ExpiresAt = epoch.Add(90 * 24 * time.Hour)
	insert(t, s, m)
	d, err := s.ChatTimer(t.Context(), chat(t, alice))
	if err != nil || d != 90*24*time.Hour {
		t.Fatalf("ChatTimer by phone number = %v, %v, want the LID chat's 90 days", d, err)
	}
}
