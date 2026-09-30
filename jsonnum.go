package gundb

import "ella.to/gundb/internal/js"

// JS-exact formatting and ordering; see package internal/js.
func jsonNumber(f float64) string { return js.Number(f) }
func jsQuote(s string) string     { return js.Quote(s) }
func jsCompare(a, b string) int   { return js.Compare(a, b) }
