package chat

import "github.com/gigovich/aigem/internal/daemonstate"

// stateFile is where the fleet daemon records itself. It is a separate file
// from the session daemon's because the two are separate processes: `aigem web
// run` serves the conversations you drive, `aigem bot start` serves the ones
// the fleet has on its own, and either may be running without the other.
const stateFile = "chat.json"

// State is how `aigem chat` finds the running fleet.
type State = daemonstate.State

// SaveState records a running fleet daemon.
func SaveState(s State) error { return daemonstate.Save(stateFile, s) }

// LoadState returns the recorded daemon, and whether it is still running.
func LoadState() (State, bool, error) { return daemonstate.Load(stateFile) }

// ClearState removes the record on a clean exit.
func ClearState() error { return daemonstate.Clear(stateFile) }
