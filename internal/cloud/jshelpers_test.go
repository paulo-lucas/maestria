package cloud

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// In the browser code, $ returns one element (or null) and $$ returns an array. Calling an array
// method on $(...) throws, and in the hosted overview that left the page stuck on an earlier
// state ("Approve this browser") while every other page worked. Catch the mix-up statically.
func TestSingleElementHelperIsNotUsedAsAList(t *testing.T) {
	bad := regexp.MustCompile(`(^|[^$\w])\$\(\s*'[^']*'\s*(,[^()]*(\([^()]*\))?[^()]*)?\)\s*\.\s*(forEach|map|filter|some|every|reduce|length)\b`)
	files, err := filepath.Glob("static/*.js")
	if err != nil || len(files) == 0 {
		t.Fatalf("no static scripts found: %v", err)
	}
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(raw), "\n") {
			if bad.MatchString(line) {
				t.Errorf("%s:%d: $(...) returns one element, use $$(...) for a list:\n\t%s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}
