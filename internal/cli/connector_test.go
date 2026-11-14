package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/chat"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/events"
	"github.com/paulo-lucas/maestria/internal/inbox"
	"github.com/paulo-lucas/maestria/internal/store"
)

func newConnectorEnv(t *testing.T, now time.Time) (*Env, string, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	root := t.TempDir()
	cfgDir := filepath.Join(root, "cfg")
	maestriDir := filepath.Join(root, "maestri")
	wsDir := filepath.Join(root, "app")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	var out, errBuf bytes.Buffer
	env := &Env{
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errBuf,
		Config: config.Env{
			Home: root,
			Getenv: func(k string) string {
				switch k {
				case "MAESTRIA_CONFIG_DIR":
					return cfgDir
				case "MAESTRI_HOME":
					return maestriDir
				default:
					return ""
				}
			},
		},
		Now: func() time.Time { return now },
	}
	return env, cfgDir, wsDir, &out, &errBuf
}

func runCLI(t *testing.T, env *Env, out, errBuf *bytes.Buffer, args ...string) string {
	t.Helper()
	out.Reset()
	errBuf.Reset()
	if code := Run(args, *env); code != 0 {
		t.Fatalf("cli %v failed with exit %d (stderr=%s stdout=%s)", args, code, errBuf.String(), out.String())
	}
	return out.String()
}

func TestCLIContractAndJSON(t *testing.T) {
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	env, cfgDir, wsDir, out, errBuf := newConnectorEnv(t, now)

	// workspace add / list
	addOut := runCLI(t, env, out, errBuf, "workspace", "add", wsDir, "--name", "Sample App", "--json")
	var added struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(addOut), &added); err != nil || added.ID != "sample-app" {
		t.Fatalf("workspace add json=%q err=%v", addOut, err)
	}
	listOut := runCLI(t, env, out, errBuf, "workspace", "list", "--json")
	if !strings.Contains(listOut, `"sample-app"`) {
		t.Fatalf("workspace list=%s", listOut)
	}

	// post notice, approve, choice (flags placed after positional args matching V1)
	noticeOut := runCLI(t, env, out, errBuf, "post", "notice", "Started checks", "--workspace", "sample-app", "--json")
	var noticeItem inbox.Item
	if err := json.Unmarshal([]byte(noticeOut), &noticeItem); err != nil || noticeItem.Kind != "notice" {
		t.Fatalf("post notice json=%q err=%v", noticeOut, err)
	}

	choiceOut := runCLI(t, env, out, errBuf, "post", "choice", "Target env?", "--option", "Staging", "--option", "Prod", "--author", "Maestro", "--workspace", "sample-app", "--json")
	var choiceItem inbox.Item
	if err := json.Unmarshal([]byte(choiceOut), &choiceItem); err != nil || len(choiceItem.Options) != 2 {
		t.Fatalf("post choice json=%q err=%v", choiceOut, err)
	}

	// inbox --json shows both open tasks
	inboxOut := runCLI(t, env, out, errBuf, "inbox", "--workspace", "sample-app", "--json")
	if !strings.Contains(inboxOut, "Started checks") || !strings.Contains(inboxOut, "Target env?") {
		t.Fatalf("inbox json=%s", inboxOut)
	}

	// Human responds directly in DB (simulating web panel while serve may or may not run).
	db, err := store.Open(cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := inbox.Respond(db, "sample-app", choiceItem.ID, "Staging", now.Add(time.Second).UnixMilli()); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	// answers --ack --json returns choiceItem once, second run returns empty.
	ansOut := runCLI(t, env, out, errBuf, "answers", "--workspace", "sample-app", "--ack", "--json")
	if !strings.Contains(ansOut, `"Staging"`) {
		t.Fatalf("answers=%s", ansOut)
	}
	ansOut2 := runCLI(t, env, out, errBuf, "answers", "--workspace", "sample-app", "--json")
	if strings.Contains(ansOut2, `"Staging"`) {
		t.Fatalf("expected acked answer hidden, got %s", ansOut2)
	}

	// say emits NO event
	sayOut := runCLI(t, env, out, errBuf, "say", "Deploying now", "--workspace", "sample-app", "--json")
	if !strings.Contains(sayOut, `"sender": "maestro"`) {
		t.Fatalf("say=%s", sayOut)
	}

	// wait --since 0 immediately gets the inbox_response event, does not see maestro say.
	waitOut := runCLI(t, env, out, errBuf, "wait", "--workspace", "sample-app", "--since", "0", "--cursor", "c1", "--json")
	var waitRes struct {
		Events []events.Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(waitOut), &waitRes); err != nil || len(waitRes.Events) != 1 || waitRes.Events[0].Kind != events.KindInboxResponse {
		t.Fatalf("wait json=%q err=%v", waitOut, err)
	}

	// wait --from-now with short timeout exits cleanly with timedOut: true.
	waitTO := runCLI(t, env, out, errBuf, "wait", "--workspace", "sample-app", "--from-now", "--timeout", "40ms", "--json")
	if !strings.Contains(waitTO, `"timedOut": true`) {
		t.Fatalf("expected timedOut: true, got %s", waitTO)
	}

	// status --json works cleanly and reports server reachability.
	statusOut := runCLI(t, env, out, errBuf, "status", "--workspace", "sample-app", "--json")
	if !strings.Contains(statusOut, `"needsYou": 0`) || !strings.Contains(statusOut, `"updates": 1`) || !strings.Contains(statusOut, `"server"`) {
		t.Fatalf("status=%s", statusOut)
	}

	// V1 flag contract parity: --kind/-k, -o, --by, -w, -s responded, answers --all, workspace add <name> <path>.
	v1WS := filepath.Join(filepath.Dir(wsDir), "v1-app")
	if err := os.MkdirAll(v1WS, 0o755); err != nil {
		t.Fatal(err)
	}
	runCLI(t, env, out, errBuf, "workspace", "add", "V1 App", v1WS, "--maestro", "Orin", "--json")
	v1Post := runCLI(t, env, out, errBuf, "post", "--kind", "choice", "-o", "A", "-o", "B", "--by", "Orin", "-w", "v1-app", "--json", "Pick", "branch")
	var v1Item inbox.Item
	if err := json.Unmarshal([]byte(v1Post), &v1Item); err != nil || v1Item.Kind != "choice" || v1Item.Author == nil || *v1Item.Author != "Orin" {
		t.Fatalf("v1 post=%q err=%v", v1Post, err)
	}
	v1Inbox := runCLI(t, env, out, errBuf, "inbox", "-w", "v1-app", "-s", "open", "-k", "choice", "--json")
	if !strings.Contains(v1Inbox, "Pick branch") {
		t.Fatalf("v1 inbox=%s", v1Inbox)
	}
	runCLI(t, env, out, errBuf, "say", "-w", "v1-app", "--by", "Orin", "--json", "Checking in")
	runCLI(t, env, out, errBuf, "answers", "-w", "sample-app", "--all", "--json")

	// workspace remove
	runCLI(t, env, out, errBuf, "workspace", "remove", "v1-app", "--json")
	runCLI(t, env, out, errBuf, "workspace", "remove", "sample-app", "--json")
}

func TestWaitSubprocessBlocksUntilEventArrives(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping subprocess build in short mode")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "maestria")
	build := exec.Command("go", "build", "-buildvcs=false", "-o", bin, "../../cmd/maestria")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build maestria binary: %v\n%s", err, out)
	}

	cfgDir := filepath.Join(root, "cfg")
	wsDir := filepath.Join(root, "repo")
	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	runBin := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), "MAESTRIA_CONFIG_DIR="+cfgDir, "MAESTRI_HOME="+filepath.Join(root, "empty-maestri"))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("bin %v failed: %v\n%s", args, err, out)
		}
		return string(out)
	}

	runBin("workspace", "add", wsDir, "--name", "SubWS")

	waitCmd := exec.Command(bin, "wait", "--workspace", "subws", "--from-now", "--cursor", "sub", "--timeout", "10s", "--json")
	waitCmd.Env = append(os.Environ(), "MAESTRIA_CONFIG_DIR="+cfgDir, "MAESTRI_HOME="+filepath.Join(root, "empty-maestri"))
	var stdout, stderr bytes.Buffer
	waitCmd.Stdout = &stdout
	waitCmd.Stderr = &stderr
	if err := waitCmd.Start(); err != nil {
		t.Fatal(err)
	}

	// Confirm it actually stays alive for >250ms while waiting.
	exited := make(chan error, 1)
	go func() { exited <- waitCmd.Wait() }()

	select {
	case err := <-exited:
		t.Fatalf("wait exited prematurely: err=%v stdout=%s stderr=%s", err, stdout.String(), stderr.String())
	case <-time.After(300 * time.Millisecond):
	}

	// Emit a human chat message directly into SQLite, waking up the subprocess.
	db, err := store.Open(cfgDir)
	if err != nil {
		_ = waitCmd.Process.Kill()
		t.Fatal(err)
	}
	if _, _, err := chat.Send(db, "subws", chat.SenderHuman, "Ping from human", time.Now().UnixMilli()); err != nil {
		_ = db.Close()
		_ = waitCmd.Process.Kill()
		t.Fatal(err)
	}
	_ = db.Close()

	select {
	case err := <-exited:
		if err != nil {
			t.Fatalf("wait exited with error: %v stderr=%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		_ = waitCmd.Process.Kill()
		t.Fatal("wait subprocess did not exit after event arrived")
	}

	var res struct {
		TimedOut bool           `json:"timedOut"`
		Events   []events.Event `json:"events"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("invalid wait stdout JSON %q: %v", stdout.String(), err)
	}
	if res.TimedOut || len(res.Events) != 1 || res.Events[0].Kind != events.KindChat {
		t.Fatalf("unexpected wait result: %+v", res)
	}
}
