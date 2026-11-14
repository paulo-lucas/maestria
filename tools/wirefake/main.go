// Command wirefake runs a fake Maestri Wire host with a small scripted world, and prepares a
// throwaway Maestria config directory paired with it, so the panel can be driven in a browser
// without Maestri. Development only.
//
//	go run ./tools/wirefake --dir /tmp/maestria-wirefake
//	MAESTRIA_CONFIG_DIR=/tmp/maestria-wirefake go run ./cmd/maestria serve
//
// Then sign in with the password and TOTP code it prints, and steer the world with its control
// endpoint (for example: curl 'http://127.0.0.1:4798/ctl?cmd=land').
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/maestria-dev/maestria/internal/auth"
	"github.com/maestria-dev/maestria/internal/config"
	"github.com/maestria-dev/maestria/internal/wire"
	"github.com/maestria-dev/maestria/internal/wire/wiretest"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

const (
	wsWire = "11111111-1111-4111-8111-111111111111"
	termM  = "22222222-2222-4222-8222-222222222221"
	termO  = "22222222-2222-4222-8222-222222222222"
	termV  = "22222222-2222-4222-8222-222222222223"
	termW  = "22222222-2222-4222-8222-222222222224"
	floorB = "44444444-4444-4444-8444-444444444441"
	floorI = "44444444-4444-4444-8444-444444444442"
	pw     = "wirefake-password"
)

type world struct {
	mu        sync.Mutex
	fake      *wiretest.Server
	prompt    string
	progress  float64
	floorB    bool
	floorI    bool
	maestroUp bool
	chat      []wire.ChatEntry
	chatState string
	preview   map[string][]string
	n         int
}

func (w *world) push() {
	card := func(id, name, agent, floorID, floor string, running, active bool) wire.TerminalCard {
		return wire.TerminalCard{ID: id, Name: name, AgentType: agent, FloorID: floorID, FloorName: floor, IsRunning: running, IsActive: active, Preview: w.preview[id]}
	}
	maestro := card(termM, "Maestro", "claude", "", "", w.maestroUp, true)
	maestro.IsManager = true
	orin := card(termO, "Orin", "gemini", floorB, "billing-webhooks", true, true)
	vesper := card(termV, "Vesper", "claude", floorB, "billing-webhooks", true, true)
	wren := card(termW, "Wren", "gemini", floorI, "invoice-pdf", true, false)
	items := []wire.FeedItem{{Kind: wire.ItemTerminal, Terminal: &maestro}}
	floorsMeta := []wire.FloorMeta{{Name: "main", IsActive: true}}
	if w.floorB {
		if w.prompt != "" {
			items = append(items, wire.FeedItem{Kind: wire.ItemPendingPrompt, Terminal: &orin, Prompt: w.prompt})
		} else {
			items = append(items, wire.FeedItem{Kind: wire.ItemTerminal, Terminal: &orin})
		}
		items = append(items, wire.FeedItem{Kind: wire.ItemTerminal, Terminal: &vesper})
		p := w.progress
		st := "working"
		if p >= 0.8 {
			st = "review"
		}
		floorsMeta = append(floorsMeta, wire.FloorMeta{ID: floorB, Name: "billing-webhooks", Branch: "billing-webhooks", Status: &wire.FloorStatus{State: st, Text: "Retry queue done, tests next", Progress: &p}})
	}
	if w.floorI {
		items = append(items, wire.FeedItem{Kind: wire.ItemTerminal, Terminal: &wren})
		p := 0.4
		floorsMeta = append(floorsMeta, wire.FloorMeta{ID: floorI, Name: "invoice-pdf", Branch: "invoice-pdf", Status: &wire.FloorStatus{State: "working", Text: "Templates in progress", Progress: &p}})
	}
	w.fake.SetFeed(wsWire, wire.FeedSnapshot{Workspace: wire.WorkspaceMeta{ID: wsWire, Name: "Atlas", IsLoaded: true}, Floors: floorsMeta, Items: items})
	w.fake.SetChat(termM, wire.ChatSnapshot{TerminalID: termM, Enabled: true, Status: w.chatState, Entries: w.chat})
}

func (w *world) say(role, text string) {
	w.n++
	w.chat = append(w.chat, wire.ChatEntry{ID: fmt.Sprintf("e%d", w.n), Role: role, Kind: "message", Text: text, TS: json.RawMessage(fmt.Sprint(time.Now().Unix()))})
}

func main() {
	dir := flag.String("dir", filepath.Join(os.TempDir(), "maestria-wirefake"), "config directory to prepare")
	port := flag.Int("port", 4799, "panel port written to config.json")
	ctl := flag.String("ctl", "127.0.0.1:4798", "control endpoint")
	flag.Parse()
	if err := run(*dir, *port, *ctl); err != nil {
		fmt.Fprintln(os.Stderr, "wirefake:", err)
		os.Exit(1)
	}
}

func run(dir string, port int, ctlAddr string) error {
	_ = os.RemoveAll(dir)
	wsDir := filepath.Join(dir, "atlas-project")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		return err
	}
	fake, err := wiretest.New()
	if err != nil {
		return err
	}
	defer fake.Close()
	fake.SetPassword(pw)
	fake.SetWorkspaces(wire.WorkspaceMeta{ID: wsWire, Name: "Atlas", IsLoaded: true, WorkingDirectory: wsDir})

	w := &world{fake: fake, prompt: "Allow `pnpm add zod@3.23.8`? (Y/n)", progress: 0.85, floorB: true, floorI: true, maestroUp: true, chatState: "idle",
		preview: map[string][]string{
			termM: {"Coordinating 3 floors", "Next: land billing-webhooks after your approval"},
			termO: {"$ pnpm test src/billing", "✓ webhooks/signature.test.ts (12)", "✓ webhooks/retry.test.ts (9)", "! payload shape is unchecked in handler.ts:41", "Adding runtime validation with zod.", "Install zod@3.23.8? (Y/n)"},
			termV: {"Editing src/billing/webhooks/retry.test.ts", "Running pnpm vitest retry.test.ts", "✓ 9 passed"},
			termW: {"✓ 31 passed (invoice-pdf)", "Done. Templates render in 120 ms on average.", ">"},
		}}
	w.say("agent", "Good morning. The Gemini window reset at 09:00, so I woke the floors that were paused overnight.\n\n- **billing-webhooks**: retry queue done, tests next\n- **invoice-pdf**: templates in progress")
	w.say("user", "How is billing-webhooks going? Can we land it today?")
	w.say("agent", "Yes. It is in **review** and all 214 tests pass. Orin is waiting on a Y/n to add `zod`.")
	fake.OnPrompt = func(id, text string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		if id == termM {
			w.say("user", text)
			w.chatState = "working"
			w.push()
			go func() {
				time.Sleep(1500 * time.Millisecond)
				w.mu.Lock()
				defer w.mu.Unlock()
				w.say("agent", "Got it: *"+text+"*. On it.")
				w.chatState = "idle"
				w.push()
			}()
			return
		}
		w.preview[id] = append(w.preview[id], "> "+text)
		w.push()
	}
	fake.OnAnswer = func(id string, approve bool) {
		w.mu.Lock()
		defer w.mu.Unlock()
		word := "approved"
		if !approve {
			word = "rejected"
		}
		w.preview[id] = append(w.preview[id], "You "+word+" it. Continuing.")
		w.prompt = ""
		w.push()
	}
	w.push()

	creds, err := wire.Provision(context.Background(), fake.Host, fake.Pin(), "Maestria", pw, nil)
	if err != nil {
		return err
	}
	if err := creds.Save(wire.Path(dir)); err != nil {
		return err
	}

	// A panel config the browser check can sign in to.
	secret, err := auth.GenerateTOTPSecret()
	if err != nil {
		return err
	}
	hash, err := auth.HashPassword(pw)
	if err != nil {
		return err
	}
	if err := auth.SaveSecrets(dir, &auth.Secrets{PasswordHash: hash, TOTPSecret: secret, CreatedAt: time.Now().UTC().Format(time.RFC3339)}); err != nil {
		return err
	}
	cfg := map[string]any{"port": port, "cookieSecure": false, "maestriDir": filepath.Join(dir, "maestri")}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	if err := os.WriteFile(config.FilePath(dir), b, 0o600); err != nil {
		return err
	}
	if _, err := workspaces.Add(dir, workspaces.AddInput{Name: "Atlas", Path: wsDir, MaestroAgent: "Maestro", MaestroProvider: "claude"}); err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ctl", func(rw http.ResponseWriter, r *http.Request) {
		w.mu.Lock()
		defer w.mu.Unlock()
		cmd := r.URL.Query().Get("cmd")
		switch cmd {
		case "totp":
			code, _ := auth.TOTP(secret, time.Now())
			fmt.Fprintln(rw, code)
			return
		case "prompt":
			w.prompt = r.URL.Query().Get("text")
			if w.prompt == "" {
				w.prompt = "Overwrite src/index.ts? (Y/n)"
			}
		case "progress":
			fmt.Sscanf(r.URL.Query().Get("v"), "%f", &w.progress)
		case "land":
			w.floorB, w.progress, w.prompt = false, 1, ""
		case "cancel":
			w.floorI = false
		case "close-maestro":
			w.maestroUp = false
			w.chatState = "notRunning"
		case "open-maestro":
			w.maestroUp = true
			w.chatState = "idle"
		case "off":
			fake.Close()
			fmt.Fprintln(rw, "Wire is off")
			return
		case "rotate":
			fake.RotateCert()
		case "revoke":
			fake.RevokeAll()
		default:
			http.Error(rw, "unknown cmd", 400)
			return
		}
		w.push()
		fmt.Fprintln(rw, "ok")
	})
	go func() { _ = http.ListenAndServe(ctlAddr, mux) }()

	code, _ := auth.TOTP(secret, time.Now())
	fmt.Printf("Fake Maestri Wire on %s (paired; wire.json written)\n", fake.Host)
	fmt.Printf("Config dir: %s\nPanel:      http://127.0.0.1:%d   password: %s   TOTP now: %s (curl 'http://%s/ctl?cmd=totp')\n", dir, port, pw, code, ctlAddr)
	fmt.Println("Control:    curl 'http://" + ctlAddr + "/ctl?cmd=<prompt|progress&v=0.9|land|cancel|close-maestro|open-maestro|off|rotate|revoke|totp>'")
	fmt.Println("Run the panel: MAESTRIA_CONFIG_DIR=" + dir + " go run ./cmd/maestria serve")
	select {}
}
