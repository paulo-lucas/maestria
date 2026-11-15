package cli

import (
	"bufio"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"

	"github.com/paulo-lucas/maestria/internal/audit"
	"github.com/paulo-lucas/maestria/internal/store"
)

// Human presence. A few commands change who may control this machine: approving a browser, lifting
// the kill switch, linking the machine to a cloud. They must not be something a script, or an AI
// agent that read a hostile web page, can run on its own. So each one needs an interactive
// terminal and a short random word that the command prints at that moment and the person types
// back. There is deliberately no flag that skips it (--yes included).
//
// Limit: an agent that owns a real pseudo-terminal can still read the word and type it. The word
// being random, and every confirmation (or failure) being written to the audit log, is what makes
// that visible. It is friction and a trail, not a lock.

var errNeedsHuman = errors.New("this changes who can control this machine, so it must be run by you in an interactive terminal (not from a script, a pipe or an agent)")

const (
	wordConsonants = "bdfgklmnprstvz"
	wordVowels     = "aeiou"
)

// confirmWord is the word to type: three consonant-vowel syllables, like "bakomi".
func (e *Env) confirmWord() string {
	if e.ConfirmWord != nil {
		return e.ConfirmWord()
	}
	b := make([]byte, 6)
	if _, err := rand.Read(b); err != nil {
		panic("maestria: no randomness: " + err.Error())
	}
	var sb strings.Builder
	for i := 0; i < 3; i++ {
		sb.WriteByte(wordConsonants[int(b[2*i])%len(wordConsonants)])
		sb.WriteByte(wordVowels[int(b[2*i+1])%len(wordVowels)])
	}
	return sb.String()
}

func auditCLI(log *audit.Log, event, action string) {
	if log != nil {
		_ = log.Write(event, "", map[string]any{"action": action})
	}
}

// needTTY is the first half of requireHuman, for commands that want to refuse before they look
// at anything else (so a script cannot use them to probe).
func (e *Env) needTTY(action string, log *audit.Log) error {
	if e.isTTY() {
		return nil
	}
	auditCLI(log, "cli_confirm_refused", action)
	return errNeedsHuman
}

// requireHuman refuses unless stdin is an interactive terminal, prints what is about to happen
// (details) and a fresh random word, and returns nil only when the person typed that word. The
// outcome is written to the audit log (log may be nil only where no database is open yet).
func (e *Env) requireHuman(in *bufio.Reader, action string, details []string, log *audit.Log) error {
	record := func(event string) { auditCLI(log, event, action) }
	if err := e.needTTY(action, log); err != nil {
		return err
	}
	word := e.confirmWord()
	fmt.Fprintf(e.Stdout, "\n%s\n", action)
	for _, d := range details {
		fmt.Fprintf(e.Stdout, "  %s\n", d)
	}
	fmt.Fprintf(e.Stdout, "\nTo go on, type this word: %s\n> ", word)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		record("cli_confirm_failed")
		return errors.New("cancelled: no confirmation was typed")
	}
	if !strings.EqualFold(strings.TrimSpace(line), word) {
		record("cli_confirm_failed")
		return errors.New("cancelled: that is not the word shown above")
	}
	record("cli_confirmed")
	return nil
}

// openAudit opens the audit log of the config directory for a command that has no database open.
// It returns a nil log (and a no-op close) when the database cannot be opened, so a confirmation
// is never blocked by that.
func openAudit(env *Env, dir string) (*audit.Log, func()) {
	db, err := store.Open(dir)
	if err != nil {
		return nil, func() {}
	}
	return audit.New(db, env.Now), func() { db.Close() }
}
