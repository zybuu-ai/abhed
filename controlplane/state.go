package controlplane

import (
	"fmt"
	"time"
)

// State is where a sandbox is in its lifecycle.
type State string

const (
	StateProvisioning State = "Provisioning"
	StateReady        State = "Ready"
	StateStopping     State = "Stopping"
	StateStopped      State = "Stopped"
	StateStarting     State = "Starting"
	StateCompleted    State = "Completed"
	StateError        State = "Error"
	StateDeleting     State = "Deleting"
)

// States lists every state.
var States = []State{StateProvisioning, StateReady, StateStopping, StateStopped,
	StateStarting, StateCompleted, StateError, StateDeleting}

// transitions is the whole lifecycle: a move not listed here is refused.
var transitions = map[State][]State{
	StateProvisioning: {StateReady, StateError, StateDeleting},
	StateReady:        {StateStopping, StateCompleted, StateError, StateDeleting},
	StateStopping:     {StateStopped, StateError, StateDeleting},
	StateStopped:      {StateStarting, StateDeleting},
	StateStarting:     {StateReady, StateError, StateDeleting},
	StateCompleted:    {StateStarting, StateDeleting},
	StateError:        {StateStarting, StateDeleting},
	StateDeleting:     nil,
}

// Valid reports whether s is a known state.
func (s State) Valid() bool {
	_, ok := transitions[s]
	return ok
}

// CanTransition reports whether a sandbox in from may move to to.
func CanTransition(from, to State) bool {
	for _, t := range transitions[from] {
		if t == to {
			return true
		}
	}
	return false
}

// AcceptsWork reports whether exec, attach, ssh, forward, expose and cp
// may run in a sandbox in s. Only Ready does; nothing queues.
func AcceptsWork(s State) bool { return s == StateReady }

// Transition moves s to state to, or refuses with ErrInvalidTransition and
// leaves s unchanged. Starting again from Stopped, Completed or Error is a
// new incarnation, so it bumps the generation.
func (s *Sandbox) Transition(to State, now time.Time) error {
	if !CanTransition(s.State, to) {
		return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, s.State, to)
	}
	if to == StateStarting {
		s.Generation++
	}
	s.State, s.UpdatedAt = to, now
	return nil
}
