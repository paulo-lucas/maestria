package wire

import (
	"fmt"
	"regexp"
	"strings"
)

// Scope says which token a client holds and so which requests it may make.
type Scope int

const (
	// ScopeRead is the guest token: reads and subscriptions. It never writes.
	ScopeRead Scope = iota + 1
	// ScopeWrite is the owner token. It is used for the allowlisted writes below and nothing
	// else, in particular never for a feed subscription (an owner subscription wakes terminals).
	ScopeWrite
	// ScopeSetup exists only for `maestria wire pair`: it pairs, and demotes the device it just
	// paired to guest. The running panel never builds a client with it.
	ScopeSetup
)

const uuid = `[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`

type rule struct {
	method string
	path   *regexp.Regexp
	scopes []Scope
	self   bool // the path's device id must be the caller's own
}

func r(method, pattern string, self bool, scopes ...Scope) rule {
	return rule{method: method, path: regexp.MustCompile(`^` + pattern + `$`), scopes: scopes, self: self}
}

// rules is the whole list of Wire routes Maestria may call. Every request the client makes goes
// through Check; anything not listed is refused before it leaves the process. WebSocket upgrades
// are GET requests and are listed as such.
var rules = []rule{
	// Reads and subscriptions (guest).
	r("GET", `/api/info`, false, ScopeRead, ScopeWrite, ScopeSetup),
	r("GET", `/api/workspaces`, false, ScopeRead),
	r("GET", `/api/workspaces/`+uuid+`/feed`, false, ScopeRead),
	r("GET", `/api/feed/stream`, false, ScopeRead),
	r("GET", `/api/terminals/`+uuid+`/chat`, false, ScopeRead),
	r("GET", `/api/terminals/`+uuid+`/chat/stream`, false, ScopeRead),

	// Writes (owner): text prompts, Y/n answers, and the terminal stream used only to send Esc.
	r("POST", `/api/terminals/`+uuid+`/prompt`, false, ScopeWrite),
	r("POST", `/api/terminals/`+uuid+`/approve`, false, ScopeWrite),
	r("POST", `/api/terminals/`+uuid+`/reject`, false, ScopeWrite),
	r("GET", `/api/terminals/`+uuid+`/stream`, false, ScopeWrite),

	// A device may revoke itself: `maestria wire unpair`.
	r("DELETE", `/api/devices/`+uuid, true, ScopeRead, ScopeWrite),

	// Pairing (setup only).
	r("POST", `/pair`, false, ScopeSetup),
	r("PUT", `/api/devices/`+uuid+`/role`, false, ScopeSetup),
}

// Check reports whether a request is allowed. method is the HTTP method, path the URL path
// without query or fragment, selfID the caller's own device id (for self-revocation).
func Check(scope Scope, method, path, selfID string) error {
	if strings.ContainsAny(path, "?#") || strings.Contains(path, "..") || strings.Contains(path, "//") {
		return fmt.Errorf("%w: %s %s", ErrNotAllowed, method, "malformed path")
	}
	for _, ru := range rules {
		if ru.method != method || !ru.path.MatchString(path) {
			continue
		}
		okScope := false
		for _, s := range ru.scopes {
			if s == scope {
				okScope = true
			}
		}
		if !okScope {
			break
		}
		if ru.self {
			id := path[strings.LastIndex(path, "/")+1:]
			if selfID == "" || !strings.EqualFold(id, selfID) {
				break
			}
		}
		return nil
	}
	return fmt.Errorf("%w: %s %s", ErrNotAllowed, method, path)
}

// esc is the only byte Maestria ever sends into a terminal: Escape, which interrupts an agent.
const esc = 0x1b

// CheckInput refuses any terminal input other than a single Escape.
func CheckInput(b []byte) error {
	if len(b) == 1 && b[0] == esc {
		return nil
	}
	return fmt.Errorf("%w: terminal input other than Esc", ErrNotAllowed)
}
