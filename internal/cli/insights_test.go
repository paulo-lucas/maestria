package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/paulo-lucas/maestria/internal/insights"
)

func TestInsightsCommand(t *testing.T) {
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	env, _, _, out, errBuf := newConnectorEnv(t, now)

	text := runCLI(t, env, out, errBuf, "insights")
	if !strings.Contains(text, "Overall: not rated yet") || !strings.Contains(text, "Delivery speed: not rated yet") {
		t.Fatalf("empty insights text = %q", text)
	}

	raw := runCLI(t, env, out, errBuf, "insights", "--json")
	var rep insights.Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil || len(rep.Areas) != 4 || rep.Level != 0 {
		t.Fatalf("insights --json = %q (err %v)", raw, err)
	}

	if code := Run([]string{"insights", "extra"}, *env); code == 0 {
		t.Fatal("insights with a stray argument must fail")
	}
}
