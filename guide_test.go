package gundb_test

import (
	"os"
	"regexp"
	"testing"
)

// TestGuideMatchesExamples keeps every program in GUIDE.md identical to the
// runnable file it names, so the guide only ever shows code that compiles.
func TestGuideMatchesExamples(t *testing.T) {
	guide, err := os.ReadFile("GUIDE.md")
	if err != nil {
		t.Fatal(err)
	}
	blocks := regexp.MustCompile("(?s)<!-- example: (\\S+) -->\n```go\n(.*?)```").FindAllSubmatch(guide, -1)
	if len(blocks) == 0 {
		t.Fatal("no examples found in GUIDE.md")
	}
	for _, b := range blocks {
		src, err := os.ReadFile(string(b[1]))
		if err != nil {
			t.Errorf("GUIDE.md: %v", err)
			continue
		}
		if string(src) != string(b[2]) {
			t.Errorf("GUIDE.md is out of date for %s", b[1])
		}
	}
}
