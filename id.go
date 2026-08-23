package gundb

import (
	"math/rand/v2"
	"strconv"
	"time"
)

// idChars is the alphabet of String.random in gun/src/shim.js.
const idChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXZabcdefghijklmnopqrstuvwxyz"

// randomID returns an n-character random string (message IDs use 9).
func randomID(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = idChars[rand.N(len(idChars))]
	}
	return string(b)
}

// uuid returns a new soul for Set items, shaped like GUN's opt.uuid: the
// current time in base 36 followed by random characters, so souls created
// later sort later.
func uuid() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 36) + randomID(12)
}
