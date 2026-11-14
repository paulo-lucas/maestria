// Command ciannotate reads `go test -json` from stdin, prints the plain test output, and emits a
// GitHub Actions error annotation for every failing test (with its last output lines), so CI
// failures are readable from the run's annotations. It exits 1 if any test or package failed.
//
//	go test -json ./... | go run ./tools/ciannotate
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type event struct {
	Action  string
	Package string
	Test    string
	Output  string
}

const keepLines = 15

func main() {
	tails := map[string][]string{}
	failed := false
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var ev event
		if err := json.Unmarshal(line, &ev); err != nil {
			fmt.Println(string(line)) // build errors and other non-JSON lines
			if strings.HasPrefix(string(line), "FAIL") || strings.Contains(string(line), "build failed") {
				failed = true
			}
			continue
		}
		key := ev.Package + " " + ev.Test
		switch ev.Action {
		case "output":
			fmt.Print(ev.Output)
			t := append(tails[key], strings.TrimRight(ev.Output, "\n"))
			if len(t) > keepLines {
				t = t[len(t)-keepLines:]
			}
			tails[key] = t
		case "fail":
			failed = true
			name := ev.Package
			if ev.Test != "" {
				name += "." + ev.Test
			}
			msg := strings.Join(tails[key], "\n")
			fmt.Printf("::error title=FAIL %s::%s\n", escape(name), escape(msg))
		}
	}
	if failed {
		os.Exit(1)
	}
}

// escape encodes a value for a workflow command (see GitHub's toolkit escapeData).
func escape(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	s = strings.ReplaceAll(s, "\n", "%0A")
	return s
}
