package chat

import (
	"testing"
	"time"
)

func TestClientFramesStartsOnceAndDetachBeforeStart(t *testing.T) {
	h := NewHub()
	c, err := h.Attach("human:operator", 0, func(uint64) ([]Frame, error) {
		return []Frame{{Seq: 1, To: []string{"human:operator"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.Detach()
	first := c.Frames()
	second := c.Frames()
	if first != second {
		t.Fatal("Frames returned different channels")
	}
	select {
	case _, ok := <-first:
		if ok {
			t.Fatal("detached client delivered a frame")
		}
	case <-time.After(time.Second):
		t.Fatal("detached client channel did not close")
	}
}

func TestHubAttachSplicesBacklogAndConcurrentPublish(t *testing.T) {
	h := NewHub()
	started := make(chan struct{})
	release := make(chan struct{})
	attached := make(chan *Client, 1)
	history := []Frame{{Seq: 1, Stream: StreamMessage, To: []string{"human:operator"}}}
	go func() {
		c, err := h.Attach("human:operator", 0, func(uint64) ([]Frame, error) {
			close(started)
			<-release
			return history, nil
		})
		if err != nil {
			t.Errorf("Attach: %v", err)
			return
		}
		attached <- c
	}()
	<-started
	h.Publish([]Frame{{Seq: 2, Stream: StreamMessage, To: []string{"human:operator"}}})
	close(release)
	c := <-attached
	defer c.Detach()
	for _, want := range []uint64{1, 2} {
		select {
		case got := <-c.Frames():
			if got.Seq != want {
				t.Fatalf("frame sequence = %d, want %d", got.Seq, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for frame %d", want)
		}
	}
}
