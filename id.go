package gundb

import (
	"math/rand/v2"
	"strconv"
	"sync"
	"time"
)

// idChars is the alphabet of String.random in gun/src/shim.js. It is in
// ASCII order, so counters written in it sort like numbers.
const idChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXZabcdefghijklmnopqrstuvwxyz"

// randomID returns an n-character random string (message IDs use 9).
func randomID(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = idChars[rand.N(len(idChars))]
	}
	return string(b)
}

// seqMax is how many souls one millisecond can name in order: a
// 3-character counter in idChars.
const seqMax = len(idChars) * len(idChars) * len(idChars)

// soulClock orders the souls uuid makes in one process.
type soulClock struct {
	mu  sync.Mutex
	ms  int64 // time of the last soul, never decreasing
	seq int   // counter of the last soul within ms
}

var soulTime soulClock

// next returns the time and counter of a new soul. Souls made in the same
// millisecond as the last one, or after the clock went back, count up from
// it; when the counter is full they move on to the next millisecond. So
// souls never sort out of order.
func (c *soulClock) next(now int64) (ms int64, seq int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case now > c.ms:
		c.ms, c.seq = now, 0
	case c.seq+1 < seqMax:
		c.seq++
	default:
		c.ms, c.seq = c.ms+1, 0
	}
	return c.ms, c.seq
}

// uuid returns a new soul for Set items, shaped like GUN's opt.uuid: the
// current time in base 36 followed by 12 characters, so souls created
// later sort later. The first 3 of those count the souls made in the same
// millisecond, which keeps a burst of Sets in order; the other 9 are
// random.
func uuid() string { return soulTime.soul(time.Now().UnixMilli()) }

func (c *soulClock) soul(now int64) string {
	ms, seq := c.next(now)
	n := len(idChars)
	return strconv.FormatInt(ms, 36) +
		string([]byte{idChars[seq/(n*n)], idChars[seq/n%n], idChars[seq%n]}) +
		randomID(9)
}
