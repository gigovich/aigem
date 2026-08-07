package tui

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// readChunk is small enough that a payload of a few hundred bytes is guaranteed
// to be cut mid-escape-sequence, whatever buffer the reader is handed.
const readChunk = 100

// chunkedReader hands out its payload in readChunk-sized pieces, reproducing a
// tty read that ends in the middle of an escape sequence. Once drained it blocks,
// because returning EOF would tear the program down before the assertions run.
type chunkedReader struct {
	mu    sync.Mutex
	data  []byte
	drain chan struct{}
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	if len(r.data) == 0 {
		r.mu.Unlock()
		<-r.drain
		return 0, io.EOF
	}
	n := min(min(readChunk, len(p)), len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	r.mu.Unlock()
	return n, nil
}

// TestWheelBurstDoesNotLeakIntoInput guards the bug that motivated the Bubble Tea
// v2 upgrade. A fast scroll arrives as one dense burst of SGR wheel sequences,
// and a Read boundary lands in the middle of one of them; v1 emitted that
// sequence's tail as literal runes, which the textarea dutifully typed. Every
// sequence in the burst must reach the model as a wheel event and none as text.
//
// The assertion is on the messages the program delivers rather than on the
// textarea's contents: the startup availability probe opens a modal alert, which
// blurs the input, so an input that stayed empty would prove nothing.
func TestWheelBurstDoesNotLeakIntoInput(t *testing.T) {
	const wheelUp = "\x1b[<64;40;12M"
	const events = 25 // x 12 bytes = 300, so the burst outruns any one Read

	r := &chunkedReader{data: []byte(strings.Repeat(wheelUp, events)), drain: make(chan struct{})}
	defer close(r.drain)

	var mu sync.Mutex
	var wheel int
	var stray []string
	settled := make(chan struct{})
	p := tea.NewProgram(newTestModel(t),
		tea.WithInput(r), tea.WithOutput(io.Discard), tea.WithoutSignals(),
		tea.WithWindowSize(80, 24),
		tea.WithFilter(func(_ tea.Model, msg tea.Msg) tea.Msg {
			mu.Lock()
			defer mu.Unlock()
			switch msg := msg.(type) {
			case tea.MouseWheelMsg:
				if wheel++; wheel == events {
					close(settled)
				}
			case tea.KeyPressMsg:
				stray = append(stray, msg.String())
			}
			return msg
		}))

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := p.Run(); err != nil {
			t.Errorf("program: %v", err)
		}
	}()

	// The reader blocks once drained, so the program never ends on its own: wait
	// for the burst to be accounted for, then stop it.
	select {
	case <-settled:
	case <-time.After(10 * time.Second):
	}
	p.Quit()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(stray) > 0 {
		t.Errorf("wheel burst decoded as key presses: %q", stray)
	}
	if wheel != events {
		t.Errorf("wheel events reaching the model = %d, want %d", wheel, events)
	}
}
