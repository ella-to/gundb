package gundb

import "math"

// hamResult is the outcome of comparing one incoming field write against
// the stored one.
type hamResult int

const (
	hamDefer      hamResult = iota // from the future: hold and retry later
	hamHistorical                  // older than what we have: ignore
	hamIncoming                    // incoming wins: write it
	hamCurrent                     // stored value wins the tie-break
	hamSame                        // identical value and state: no-op
)

// ham is the Hypothetical Amnesia Machine from gun/src/root.js:
//
//	state in a later millisecond    -> defer
//	state < stored state            -> historical
//	state > stored state            -> incoming wins
//	equal state, equal JSON         -> same
//	equal state                     -> the lexically larger JSON wins
//
// The reference defers any state > now, but the fraction of a state is only
// a counter ordering several writes made in one millisecond (see stateGen),
// and the reference applies such writes a fraction of a millisecond later
// anyway. Deferring them made a write invisible to a peer that read it in
// the same millisecond, so only states from a later millisecond wait.
//
// An absent stored field is passed as current=nil, currentState=-Inf.
func ham(machine, incomingState, currentState float64, incoming, current Value) hamResult {
	switch {
	case incomingState >= math.Floor(machine)+1:
		return hamDefer
	case incomingState < currentState:
		return hamHistorical
	case incomingState > currentState:
		return hamIncoming
	}
	switch c := jsCompare(lexical(incoming), lexical(current)); {
	case c == 0:
		return hamSame
	case c < 0:
		return hamCurrent
	default:
		return hamIncoming
	}
}
