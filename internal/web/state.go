package web

import "github.com/gigovich/aigem/internal/daemonstate"

// stateFile is where this daemon records itself. The bot fleet writes its own
// beside it: the two daemons are separate and either may run without the other.
const stateFile = "web.json"

// State is how `aigem attach` finds a running daemon.
type State = daemonstate.State

// SaveState records a running daemon.
func SaveState(s State) error { return daemonstate.Save(stateFile, s) }

// LoadState returns the recorded daemon, and whether it is still running.
func LoadState() (State, bool, error) { return daemonstate.Load(stateFile) }

// ClearState removes the record on a clean exit.
func ClearState() error { return daemonstate.Clear(stateFile) }
