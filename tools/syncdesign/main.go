// Command syncdesign copies the design system's CSS and icons from design/ into internal/ui/static/.
//
//	go generate ./internal/ui          copy what changed
//	go run ./tools/syncdesign -check   fail if the copies drifted (no writes)
package main

import (
	"bytes"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/paulo-lucas/maestria/internal/ui"
)

func main() {
	design := flag.String("design", "design", "design directory (source of truth)")
	out := flag.String("out", "internal/ui/static", "directory the UI embeds")
	check := flag.Bool("check", false, "only report drift, write nothing")
	flag.Parse()

	names := make([]string, 0, len(ui.DesignAssets))
	for name := range ui.DesignAssets {
		names = append(names, name)
	}
	sort.Strings(names)

	drift := 0
	for _, name := range names {
		src := filepath.Join(*design, filepath.FromSlash(ui.DesignAssets[name]))
		dst := filepath.Join(*out, name)
		want, err := os.ReadFile(src)
		if err != nil {
			fatal("%v", err)
		}
		have, err := os.ReadFile(dst)
		if err == nil && bytes.Equal(have, want) {
			continue
		}
		drift++
		if *check {
			fmt.Fprintf(os.Stderr, "out of date: %s (from %s)\n", dst, src)
			continue
		}
		if err := os.WriteFile(dst, want, 0o644); err != nil {
			fatal("%v", err)
		}
		fmt.Printf("updated %s\n", dst)
	}
	if *check && drift > 0 {
		fatal("%d file(s) out of date: run go generate ./internal/ui", drift)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "syncdesign: "+format+"\n", args...)
	os.Exit(1)
}
