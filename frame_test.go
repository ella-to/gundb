package gundb

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// readFrameV1 is how frames were read before readFrame read them in one
// pass: json.Unmarshal per message. readFrame must decode what it decoded.
func readFrameV1(data []byte) ([]message, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, nil
	}
	var raws []json.RawMessage
	switch data[0] {
	case '[':
		if err := json.Unmarshal(data, &raws); err != nil {
			return nil, fmt.Errorf("gundb: bad batch: %w", err)
		}
	case '{':
		raws = []json.RawMessage{data}
	default:
		return nil, fmt.Errorf("gundb: bad frame starting with %q", data[0])
	}
	msgs := make([]message, 0, len(raws))
	var errs []error
	for _, r := range raws {
		var m message
		if err := json.Unmarshal(r, &m); err != nil {
			errs = append(errs, err)
			continue
		}
		msgs = append(msgs, m)
	}
	return msgs, errors.Join(errs...)
}

var frameCorpus = []string{
	`{"#":"a","put":{"x":{"_":{"#":"x",">":{"v":1}},"v":1}}}`,
	`  {"#":"a" , "get" : {"#":"x" , ".":"y"} }  `,
	`[{"#":"a","get":{"#":"x"}},{"#":"b","@":"a","put":null,"ok":{"":1}},{"#":"c","dam":"?","pid":"p"}]`,
	`[{"#":"a","put":{"x":{"_":{"#":"x",">":{}},"bad":1}}},{"#":"b","get":{"#":"y"}}]`,
	`[{"#":1},{"#":"ok"}]`,
	`[{"#":"a","get":"x"},{"#":"b","get":{"#":{"*":"chat/"},"%":100000}}]`,
	`[{"#":"a","get":{"#":{"=":"exact"}}},{"#":"b","get":{"#":"x",".":{">":"a","<":"m"}}}]`,
	`[{"#":"a","get":{"#":"x",".":1}},{"#":"b","get":{"#":5}}]`,
	`[{"#":"a","get":{"#":"x","#":"y"}},{"#":"b","get":{"#":"x","#":{"*":"p"}}}]`,
	`{"#":"a","##":-852707395,"@":"b","FOO":{"deep":[1,2,{"x":null}]},"><":"p1,p2","%":1,"err":"boom"}`,
	`[null,{"#":"a"},"str",3,[1]]`,
	`[{"#":"a","put":{"x":null}}]`,
	`[{"#":"a","put":{"x":{"_":{"#":"x",">":{"v":1}},"v":1},"x":{"_":{"#":"x",">":{"w":2}},"w":2}}}]`,
	`[{"#":"a","put":{"x":{"_":{"#":"y",">":{"v":1}},"v":{"#":"z"}}}},{"#":"b","put":{"x":{"_":{"#":"x",">":{"v":1}},"v":[1]}}}]`,
	`{"#":"a","ok":true,"ok":false}`,
	`{"#":"a","dam":null,"pid":null,"@":null,"get":null}`,
	`[{"#":"a"}`,
	`[{"#":"a"}] x`,
	`{"#":"a"} {"#":"b"}`,
	`[{"#":"a","put":{"x":{"_":{"#":"x",">":{"v":1}},"v":1}}},{"#":"b","put":{"x":{"_":{"#":"x",">":{"v":1}},"v":`,
	`[]`,
	`x`,
	``,
	`{"#":"a\u0000b","put":{"a\u00e9":{"_":{"#":"a\u00e9",">":{"k\"q":1}},"k\"q":"v\\n"}}}`,
}

func TestReadFrameMatchesV1(t *testing.T) {
	for _, f := range append(frameCorpus, jsFrames...) {
		want, wantErr := readFrameV1([]byte(f))
		got, gotErr := readFrame([]byte(f))
		if (wantErr == nil) != (gotErr == nil) {
			t.Fatalf("%s:\nerror %v, v1 error %v", f, gotErr, wantErr)
		}
		if len(got) != len(want) {
			t.Fatalf("%s:\n%d messages, v1 %d", f, len(got), len(want))
		}
		for i := range got {
			raw := got[i].raw
			got[i].raw = nil
			if raw != nil {
				var m message
				if err := json.Unmarshal(raw, &m); err != nil || !reflect.DeepEqual(m, want[i]) {
					t.Fatalf("%s:\nmessage %d raw %s decodes to %+v (%v), want %+v", f, i, raw, m, err, want[i])
				}
			}
			if !reflect.DeepEqual(got[i], want[i]) {
				t.Fatalf("%s:\nmessage %d:\n got %+v\nwant %+v", f, i, got[i], want[i])
			}
		}
	}
}

func FuzzReadFrame(f *testing.F) {
	for _, s := range append(frameCorpus, jsFrames...) {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if caseVariantKey(s) {
			t.Skip("v1 matched keys case-insensitively; readFrame matches them exactly, like GUN.js")
		}
		want, wantErr := readFrameV1([]byte(s))
		got, gotErr := readFrame([]byte(s))
		if (wantErr == nil) != (gotErr == nil) && !strings.ContainsAny(s, "\x80\xff") {
			t.Fatalf("%q: error %v, v1 error %v", s, gotErr, wantErr)
		}
		if wantErr == nil && gotErr == nil {
			for i := range got {
				got[i].raw = nil
			}
			if !reflect.DeepEqual(got, want) && len(got)+len(want) > 0 {
				t.Fatalf("%q:\n got %+v\nwant %+v", s, got, want)
			}
		}
	})
}

// caseVariantKey reports whether s may hold a message key that json.Unmarshal
// matched to a field of message only because it ignores case, like "Put".
func caseVariantKey(s string) bool {
	lower := strings.ToLower(s)
	for _, k := range []string{`"put"`, `"get"`, `"ok"`, `"err"`, `"dam"`, `"pid"`} {
		if strings.Count(lower, k) != strings.Count(s, k) {
			return true
		}
	}
	return strings.Contains(s, "\\u") // an escaped key, compared after unescaping
}

// A put a relay passes on unchanged goes out as it came in: unknown keys
// and all, without being encoded again.
func TestRelayPassesOnReceivedBytes(t *testing.T) {
	db := newDB(t)
	sub, subConn := routePeer(t, db, false)
	sub.want("x")
	writer, _ := routePeer(t, db, false)
	frame := `[{"#":"w1","put":{"x":{"_":{"#":"x",">":{"v":1}},"v":1}},"extra":{"kept":true}},{"#":"w2","get":{"#":"y"}}]`
	msgs, err := readFrame([]byte(frame))
	if err != nil {
		t.Fatal(err)
	}
	db.handle(writer, &msgs[0])
	got := sentWithin(subConn, 100*time.Millisecond)
	if !strings.Contains(got, `"extra":{"kept":true}`) {
		t.Fatalf("not passed on as received: %s", got)
	}
	if strings.Contains(got, "w2") {
		t.Fatalf("passed on more than the message: %s", got)
	}

	// A message without an ID gets one, and is passed on with it.
	msgs, _ = readFrame([]byte(`{"put":{"x":{"_":{"#":"x",">":{"v":2}},"v":2}}}`))
	db.handle(writer, &msgs[0])
	got = sentWithin(subConn, 100*time.Millisecond)
	if !strings.Contains(got, `"#":"`+msgs[0].ID+`"`) {
		t.Fatalf("passed on without its new ID %s: %s", msgs[0].ID, got)
	}
}
