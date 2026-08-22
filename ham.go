package gundb

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
//	state > now                     -> defer
//	state < stored state            -> historical
//	state > stored state            -> incoming wins
//	equal state, equal JSON         -> same
//	equal state                     -> the lexically larger JSON wins
//
// An absent stored field is passed as current=nil, currentState=-Inf.
func ham(machine, incomingState, currentState float64, incoming, current Value) hamResult {
	switch {
	case incomingState > machine:
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
