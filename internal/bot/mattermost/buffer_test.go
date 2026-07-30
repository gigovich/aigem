package mattermost

import (
	"testing"
	"time"
)

func TestBufferAddAndRecent(t *testing.T) {
	b := newChannelBuffer(3, time.Hour)
	b.add("c1", "u1", "one")
	b.add("c1", "u2", "two")
	got := b.recent("c1")
	if len(got) != 2 || got[0].text != "one" || got[1].author != "u2" {
		t.Fatalf("recent = %+v", got)
	}
	if len(b.recent("other")) != 0 {
		t.Fatal("unknown channel should be empty")
	}
}

func TestBufferEvictsByCapacity(t *testing.T) {
	b := newChannelBuffer(2, time.Hour)
	b.add("c1", "u", "a")
	b.add("c1", "u", "b")
	b.add("c1", "u", "c")
	got := b.recent("c1")
	if len(got) != 2 || got[0].text != "b" || got[1].text != "c" {
		t.Fatalf("capacity evict = %+v", got)
	}
}

func TestBufferDropsByAge(t *testing.T) {
	now := time.Unix(1000, 0)
	b := newChannelBuffer(10, time.Hour)
	b.now = func() time.Time { return now }
	b.add("c1", "u", "old")
	now = now.Add(2 * time.Hour)
	b.add("c1", "u", "fresh")
	got := b.recent("c1")
	if len(got) != 1 || got[0].text != "fresh" {
		t.Fatalf("age drop = %+v", got)
	}
}
