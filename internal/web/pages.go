package web

import (
	"fmt"
	"html/template"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/maestria-dev/maestria/internal/chat"
	"github.com/maestria-dev/maestria/internal/inbox"
	"github.com/maestria-dev/maestria/internal/standby"
	"github.com/maestria-dev/maestria/internal/ui"
	"github.com/maestria-dev/maestria/internal/workspaces"
)

func (s *Server) populateShell(d *ui.Data, scope string) ([]workspaces.Workspace, map[string]workspaces.Workspace) {
	if scope == "" {
		scope = "all"
	}
	d.Scope = scope
	now := s.Now()
	nowMs := now.UnixMilli()
	d.NowISO = now.Format(time.RFC3339)
	d.NowLabel = "just now"

	list, _ := s.mergedWorkspaces()
	counts, _ := inbox.CountsByWorkspace(s.DB, nowMs)
	byID := make(map[string]workspaces.Workspace, len(list))
	navs := make([]ui.WorkspaceNav, 0, len(list))
	totalNeeds := 0
	for _, w := range list {
		byID[w.ID] = w
		c := counts[w.ID]
		totalNeeds += c.NeedsYou
		nav := ui.WorkspaceNav{
			ID:             w.ID,
			Name:           w.Name,
			Tone:           w.Tone,
			Initials:       workspaces.Initials(w.Name),
			Machine:        "This machine",
			NeedsCount:     c.NeedsYou,
			UpdatesCount:   c.Updates,
			PostponedCount: c.Postponed,
			TotalOpenCount: c.NeedsYou + c.Updates + c.Postponed,
			Selected:       w.ID == scope,
		}
		navs = append(navs, nav)
		if nav.Selected {
			cp := nav
			d.CurrentWS = &cp
		}
	}
	d.Workspaces = navs
	d.TotalNeeds = totalNeeds
	return list, byID
}

func (s *Server) getOverview(w http.ResponseWriter, r *http.Request) {
	d := s.appData(r, "Overview", "overview")
	list, byID := s.populateShell(d, "all")
	now := s.Now()
	nowMs := now.UnixMilli()

	wsNoun := "workspaces"
	if len(list) == 1 {
		wsNoun = "workspace"
	}
	needText := fmt.Sprintf("%d waiting on you", d.TotalNeeds)
	if d.TotalNeeds == 0 {
		needText = "nothing waiting on you"
	}
	d.SubTitle = fmt.Sprintf("%d %s on this machine · %s", len(list), wsNoun, needText)

	openItems, err := inbox.List(s.DB, inbox.ListOptions{
		Status:      inbox.StatusOpen,
		NewestFirst: true,
		NowMs:       nowMs,
	})
	if err == nil {
		for _, it := range openItems {
			if it.Kind == inbox.KindNotice || (it.PostponedUntil != nil && *it.PostponedUntil > nowMs) {
				continue
			}
			d.NeedsItems = append(d.NeedsItems, toTaskView(it, byID, now))
		}
	}

	s.populateUsage(d, now, false)
	s.render(w, ui.PageOverview, d)
}

func (s *Server) getWorkspaceTasks(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	d := s.appData(r, "Human tasks", "tasks")
	list, byID := s.populateShell(d, wsID)
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.Title = ws.Name + " · Human tasks"
	now := s.Now()
	nowMs := now.UnixMilli()

	openItems, err := inbox.List(s.DB, inbox.ListOptions{
		WorkspaceID: ws.ID,
		Status:      inbox.StatusOpen,
		NewestFirst: true,
		NowMs:       nowMs,
	})
	if err == nil {
		for _, it := range openItems {
			tv := toTaskView(it, byID, now)
			switch {
			case it.Kind == inbox.KindNotice:
				d.UpdatesItems = append(d.UpdatesItems, tv)
			case it.PostponedUntil != nil && *it.PostponedUntil > nowMs:
				d.PostponedItems = append(d.PostponedItems, tv)
			default:
				d.NeedsItems = append(d.NeedsItems, tv)
			}
		}
	}

	closedItems, err := inbox.List(s.DB, inbox.ListOptions{
		WorkspaceID: ws.ID,
		Status:      "history",
		NewestFirst: true,
		NowMs:       nowMs,
		Limit:       50,
	})
	if err == nil {
		for _, it := range closedItems {
			d.HistoryItems = append(d.HistoryItems, toTaskView(it, byID, now))
		}
	}

	s.render(w, ui.PageTasks, d)
}

func (s *Server) getWorkspaceChat(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	d := s.appData(r, "Chat", "chat")
	list, _ := s.populateShell(d, wsID)
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	d.Title = ws.Name + " · Chat"
	now := s.Now()

	msgs, err := chat.List(s.DB, ws.ID, 0, 200)
	if err == nil {
		lastDay := ""
		for _, m := range msgs {
			t := time.UnixMilli(m.CreatedAt).Local()
			day := formatDayDivider(t, now.Local())
			dayLabel := ""
			if day != lastDay {
				dayLabel = day
				lastDay = day
			}
			d.ChatMessages = append(d.ChatMessages, ui.ChatMessageView{
				ID:        m.ID,
				Author:    m.Sender,
				BodyHTML:  template.HTML(m.BodyHTML),
				TimeLabel: t.Format("15:04"),
				TimeISO:   t.Format(time.RFC3339),
				DayLabel:  dayLabel,
			})
		}
	}

	s.render(w, ui.PageChat, d)
}

func (s *Server) getWorkspaceComingSoon(nav, title, icon, text string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		wsID := r.PathValue("id")
		d := s.appData(r, title, nav)
		list, _ := s.populateShell(d, wsID)
		ws, ok := s.findWorkspace(list, wsID)
		if !ok {
			http.NotFound(w, r)
			return
		}
		d.Title = ws.Name + " · " + title
		d.ComingSoonTitle = title
		d.ComingSoonIcon = icon
		d.ComingSoonText = text
		s.render(w, ui.PageComingSoon, d)
	}
}

func (s *Server) getUsage(w http.ResponseWriter, r *http.Request) {
	d := s.appData(r, "Usage & standby", "usage")
	s.populateShell(d, "all")
	s.populateUsage(d, s.Now(), true)
	s.render(w, ui.PageUsage, d)
}

func (s *Server) populateUsage(d *ui.Data, now time.Time, includeStandbyControls bool) {
	snap, sb, _ := s.evaluateUsageAndStandby()
	cfg := s.standbyConfigCopy()

	d.UsageAvailable = snap.Available
	if snap.Issue != nil {
		d.UsageIssueTitle = snap.Issue.Title
		d.UsageIssueMessage = snap.Issue.Message
	}
	d.UsageHiddenCount = snap.HiddenCount
	d.WakeOnReset = cfg.WakeOnReset
	d.StandbyActive = sb.Active

	activeByGuard := make(map[string]standby.ActiveGuard, len(sb.Guards))
	for _, g := range sb.Guards {
		activeByGuard[g.GuardKey] = g
	}
	if includeStandbyControls && sb.Active && len(sb.Guards) > 0 {
		first := sb.Guards[0]
		text := fmt.Sprintf("Standby active since %s.", time.UnixMilli(first.EnteredAt).Local().Format("15:04"))
		if first.ResetAt != nil && *first.ResetAt > 0 {
			text = fmt.Sprintf("Maestros were notified at %s. Expected reset: %s.",
				time.UnixMilli(first.EnteredAt).Local().Format("15:04"),
				time.UnixMilli(*first.ResetAt).Local().Format("Mon 15:04"))
		}
		d.StandbyBanner = &ui.StandbyBannerView{
			Active:   true,
			GuardKey: "",
			Title:    sb.Banner,
			Text:     text,
		}
	}

	for _, h := range snap.Hidden {
		d.UsageHidden = append(d.UsageHidden, ui.HiddenProviderView{
			Key:           h.Key,
			Provider:      h.Provider,
			ProviderLabel: h.ProviderLabel,
			State:         h.State,
			Error:         h.Error,
		})
	}

	for _, acc := range snap.Accounts {
		av := ui.UsageAccountView{
			Key:           acc.Key,
			Provider:      acc.Provider,
			ProviderLabel: acc.ProviderLabel,
			AccountLabel:  acc.AccountLabel,
			Plan:          acc.Plan,
			State:         acc.State,
			Error:         acc.Error,
			Stale:         acc.Stale,
			StatusTone:    "idle",
			StatusLabel:   "Off",
			StatusBadge:   true,
		}
		parts := []string{}
		if acc.Plan != "" {
			parts = append(parts, acc.Plan)
		}
		if acc.AccountLabel != "" && acc.AccountLabel != "Default" {
			parts = append(parts, acc.AccountLabel)
		}
		if acc.Stale {
			parts = append(parts, "Cached (<24h)")
		}
		av.Subline = strings.Join(parts, " · ")

		anyArmed := false
		anyTripped := false
		for _, win := range acc.Windows {
			armed := win.Threshold > 0
			if armed {
				anyArmed = true
			}
			if win.Tripped {
				anyTripped = true
			}

			gk := acc.Key + ":" + win.ID
			g, hasGuard := activeByGuard[gk]

			tone := ""
			if win.Tripped || win.RoundedPct >= 90 {
				tone = "danger"
			} else if win.RoundedPct >= 75 || (armed && win.RoundedPct >= win.Threshold) {
				tone = "warn"
			}

			footRight := ""
			if hasGuard {
				footRight = "Paused at " + time.UnixMilli(g.EnteredAt).Local().Format("15:04")
			} else if armed {
				footRight = fmt.Sprintf("Pause at %d%%", win.Threshold)
			}

			thVal := win.Threshold
			if thVal <= 0 {
				thVal = 90
			}
			markCls := ""
			if win.MarkStep >= 50 && win.MarkStep <= 100 && win.MarkStep%5 == 0 {
				markCls = fmt.Sprintf("mark-%d", win.MarkStep)
			}

			safeID := sanitizeDOMID(acc.Key + "-" + win.ID)
			wv := ui.UsageWindowView{
				AccountKey:     acc.Key,
				ID:             win.ID,
				Label:          win.Label,
				UsedPct:        win.RoundedPct,
				ResetLabel:     win.ResetLabel,
				Threshold:      win.Threshold,
				ThresholdValue: thVal,
				MarkClass:      markCls,
				Action:         win.Action,
				Armed:          armed,
				Tripped:        win.Tripped,
				WakeOnReset:    cfg.WakeOnReset,
				StatusTone:     tone,
				FootRight:      footRight,
				SwitchID:       "sb-on-" + safeID,
				SelectID:       "sb-pct-" + safeID,
				RadioName:      "sb-act-" + safeID,
				ThresholdOpts:  buildThresholdOptions(thVal),
			}
			av.Windows = append(av.Windows, wv)
		}

		switch {
		case anyTripped:
			av.StatusTone = "warn"
			av.StatusLabel = "Paused"
		case acc.Stale:
			av.StatusTone = "warn"
			av.StatusLabel = "Stale"
		case anyArmed:
			av.StatusTone = "ok"
			av.StatusLabel = "Armed"
		default:
			av.StatusTone = "idle"
			av.StatusLabel = "Off"
		}
		d.UsageAccounts = append(d.UsageAccounts, av)
	}
}

func buildThresholdOptions(selected int) []ui.ThresholdOption {
	vals := []int{50, 55, 60, 65, 70, 75, 80, 85, 90, 95, 100}
	seen := false
	for _, v := range vals {
		if v == selected {
			seen = true
			break
		}
	}
	if !seen && selected > 0 && selected <= 100 {
		vals = append(vals, selected)
		sort.Ints(vals)
	}
	out := make([]ui.ThresholdOption, 0, len(vals))
	for _, v := range vals {
		out = append(out, ui.ThresholdOption{
			Value:    v,
			Label:    fmt.Sprintf("%d%%", v),
			Selected: v == selected,
		})
	}
	return out
}

func sanitizeDOMID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return b.String()
}

func toTaskView(it inbox.Item, byID map[string]workspaces.Workspace, now time.Time) ui.TaskView {
	wsName := it.WorkspaceID
	wsTone := "blue"
	if w, ok := byID[it.WorkspaceID]; ok {
		wsName = w.Name
		wsTone = w.Tone
	}

	postponeLabel := ""
	if it.PostponedUntil != nil && *it.PostponedUntil > now.UnixMilli() {
		postponeLabel = formatWakeTime(*it.PostponedUntil, now)
	}

	respStr := ""
	if it.Response != nil {
		respStr = *it.Response
	}
	authorStr := ""
	if it.Author != nil {
		authorStr = *it.Author
	}

	statusTone, statusLabel := closedStatusLabel(it, respStr)
	resolvedLabel := ""
	if it.AnsweredAt != nil && *it.AnsweredAt > 0 {
		resolvedLabel = time.UnixMilli(*it.AnsweredAt).Local().Format("Jan 2, 15:04")
	}

	// For approve items, RespondWithComment stores "Yes — <comment>" or "No — <comment>".
	displayResp := respStr
	if it.Kind == inbox.KindApprove {
		if idx := strings.Index(respStr, " — "); idx >= 0 {
			displayResp = strings.TrimSpace(respStr[idx+len(" — "):])
		} else if strings.EqualFold(respStr, "Yes") || strings.EqualFold(respStr, "No") {
			displayResp = ""
		}
	}

	kindIcon, kindLabel := taskKindMeta(it.Kind)
	return ui.TaskView{
		ID:            it.ID,
		WorkspaceID:   it.WorkspaceID,
		WorkspaceName: wsName,
		WorkspaceTone: wsTone,
		Kind:          it.Kind,
		KindLabel:     kindLabel,
		KindIcon:      kindIcon,
		By:            authorStr,
		Title:         it.Title,
		BodyHTML:      template.HTML(it.BodyHTML),
		Options:       it.Options,
		Status:        it.Status,
		Response:      displayResp,
		StatusTone:    statusTone,
		StatusLabel:   statusLabel,
		PostponeLabel: postponeLabel,
		CreatedRel:    formatRelativeTime(it.CreatedAt, now),
		CreatedISO:    time.UnixMilli(it.CreatedAt).Format(time.RFC3339),
		ResolvedLabel: resolvedLabel,
	}
}

func taskKindMeta(kind string) (string, string) {
	switch kind {
	case inbox.KindApprove:
		return "i-approve", "Approval"
	case inbox.KindChoice:
		return "i-choice", "Choice"
	case inbox.KindInput:
		return "i-question", "Question"
	case inbox.KindNotice:
		return "i-bell", "Update"
	default:
		return "i-inbox", "Task"
	}
}

func closedStatusLabel(it inbox.Item, resp string) (string, string) {
	switch it.Status {
	case inbox.StatusDismissed:
		return "idle", "Read"
	case inbox.StatusAnswered:
		switch it.Kind {
		case inbox.KindApprove:
			low := strings.ToLower(strings.TrimSpace(resp))
			if strings.HasPrefix(low, "yes") || strings.HasPrefix(low, "approve") {
				return "done", "Approved"
			}
			return "blocked", "Rejected"
		case inbox.KindChoice:
			return "idle", "Chose"
		default:
			return "done", "Answered"
		}
	default:
		return "idle", it.Status
	}
}

func formatDayDivider(t, now time.Time) string {
	ty, tm, td := t.Date()
	ny, nm, nd := now.Date()
	if ty == ny && tm == nm && td == nd {
		return "Today"
	}
	yes := now.AddDate(0, 0, -1)
	yy, ym, yd := yes.Date()
	if ty == yy && tm == ym && td == yd {
		return "Yesterday"
	}
	return t.Format("Jan 2")
}
