package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/paulo-lucas/maestria/internal/chat"
	"github.com/paulo-lucas/maestria/internal/config"
	"github.com/paulo-lucas/maestria/internal/docs"
	"github.com/paulo-lucas/maestria/internal/floors"
	"github.com/paulo-lucas/maestria/internal/inbox"
	"github.com/paulo-lucas/maestria/internal/standby"
	"github.com/paulo-lucas/maestria/internal/usage"
	"github.com/paulo-lucas/maestria/internal/workspaces"
)

type apiWorkspace struct {
	workspaces.Workspace
	Initials string                `json:"initials"`
	Counts   inbox.WorkspaceCounts `json:"counts"`
}

type apiInboxItem struct {
	inbox.Item
	WorkspaceName  string `json:"workspaceName"`
	WorkspaceColor string `json:"workspaceColor"`
	WorkspaceTone  string `json:"workspaceTone"`
	RelTime        string `json:"relTime"`
	TimeISO        string `json:"timeIso"`
	PostponeLabel  string `json:"postponeLabel,omitempty"`
}

func (s *Server) mergedWorkspaces() ([]workspaces.Workspace, error) {
	s.cfgMu.RLock()
	cfgDir := s.Config.ConfigDir
	maestriDir := s.Config.MaestriDir
	s.cfgMu.RUnlock()
	return workspaces.Merged(cfgDir, maestriDir, s.env.Home)
}

func (s *Server) findWorkspace(list []workspaces.Workspace, id string) (workspaces.Workspace, bool) {
	for _, w := range list {
		if w.ID == id {
			return w, true
		}
	}
	return workspaces.Workspace{}, false
}

func (s *Server) standbyConfigCopy() config.Standby {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return cloneStandbyConfig(s.Config.Standby)
}

func (s *Server) configCopy() *config.Config {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	cp := *s.Config
	cp.Standby = cloneStandbyConfig(s.Config.Standby)
	return &cp
}

func cloneStandbyConfig(sb config.Standby) config.Standby {
	out := config.Standby{
		Thresholds:   map[string]map[string]int{},
		Actions:      map[string]map[string]string{},
		WakeOnReset:  sb.WakeOnReset,
		NotifyViaCLI: sb.NotifyViaCLI,
	}
	if sb.ResumeBelowPercent != nil {
		v := *sb.ResumeBelowPercent
		out.ResumeBelowPercent = &v
	}
	for k, wm := range sb.Thresholds {
		m := make(map[string]int, len(wm))
		for wk, val := range wm {
			m[wk] = val
		}
		out.Thresholds[k] = m
	}
	for k, am := range sb.Actions {
		m := make(map[string]string, len(am))
		for wk, val := range am {
			m[wk] = val
		}
		out.Actions[k] = m
	}
	return out
}

func decodeJSONBody(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	return nil
}

func (s *Server) enrichInboxItem(it inbox.Item, byID map[string]workspaces.Workspace, now time.Time) apiInboxItem {
	ws := byID[it.WorkspaceID]
	name := ws.Name
	if name == "" {
		name = it.WorkspaceID
	}
	tone := ws.Tone
	if tone == "" {
		tone = "blue"
	}
	tsMs := it.CreatedAt
	if it.Status != inbox.StatusOpen && it.AnsweredAt != nil {
		tsMs = *it.AnsweredAt
	}
	out := apiInboxItem{
		Item:           it,
		WorkspaceName:  name,
		WorkspaceColor: ws.Color,
		WorkspaceTone:  tone,
		RelTime:        formatRelativeTime(tsMs, now),
		TimeISO:        time.UnixMilli(tsMs).UTC().Format(time.RFC3339),
	}
	if it.Postponed && it.PostponedUntil != nil {
		out.PostponeLabel = formatWakeTime(*it.PostponedUntil, now)
	}
	return out
}

func (s *Server) apiWorkspaces(w http.ResponseWriter, r *http.Request) {
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	counts, err := inbox.CountsByWorkspace(s.DB, s.Now().UnixMilli())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	sb, _ := standby.Current(s.DB)

	out := make([]apiWorkspace, 0, len(list))
	for _, ws := range list {
		out = append(out, apiWorkspace{
			Workspace: ws,
			Initials:  workspaces.Initials(ws.Name),
			Counts:    counts[ws.ID],
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspaces": out,
		"standby":    sb,
	})
}

func (s *Server) apiInbox(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	wsFilter := strings.TrimSpace(q.Get("workspace"))
	status := strings.TrimSpace(q.Get("status"))
	if status == "" {
		status = inbox.StatusOpen
	} else if status == "responded" {
		status = inbox.StatusAnswered
	}
	switch status {
	case inbox.StatusOpen, inbox.StatusAnswered, inbox.StatusDismissed, "history", "all":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
		return
	}
	kind := strings.TrimSpace(q.Get("kind"))

	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	byID := make(map[string]workspaces.Workspace, len(list))
	for _, ws := range list {
		byID[ws.ID] = ws
	}
	if wsFilter != "" {
		if _, ok := byID[wsFilter]; !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
			return
		}
	}
	s.serveInboxList(w, wsFilter, status, kind, byID, list)
}

func (s *Server) apiWorkspaceInbox(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
		return
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	if status == "" {
		status = inbox.StatusOpen
	} else if status == "responded" {
		status = inbox.StatusAnswered
	}
	switch status {
	case inbox.StatusOpen, inbox.StatusAnswered, inbox.StatusDismissed, "history", "all":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_status"})
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("kind"))
	byID := map[string]workspaces.Workspace{ws.ID: ws}
	s.serveInboxList(w, ws.ID, status, kind, byID, []workspaces.Workspace{ws})
}

func (s *Server) serveInboxList(
	w http.ResponseWriter,
	wsFilter, status, kind string,
	byID map[string]workspaces.Workspace,
	scopeList []workspaces.Workspace,
) {
	now := s.Now()
	nowMs := now.UnixMilli()

	if status != inbox.StatusOpen {
		items, err := inbox.List(s.DB, inbox.ListOptions{
			WorkspaceID: wsFilter,
			Status:      status,
			Kind:        kind,
			NewestFirst: true,
			NowMs:       nowMs,
		})
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		enriched := make([]apiInboxItem, 0, len(items))
		for _, it := range items {
			enriched = append(enriched, s.enrichInboxItem(it, byID, now))
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": enriched})
		return
	}

	openItems, err := inbox.List(s.DB, inbox.ListOptions{
		WorkspaceID: wsFilter,
		Status:      inbox.StatusOpen,
		Kind:        kind,
		NewestFirst: true,
		NowMs:       nowMs,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	needsYou := make([]apiInboxItem, 0)
	postponed := make([]apiInboxItem, 0)
	updates := make([]apiInboxItem, 0)
	for _, it := range openItems {
		en := s.enrichInboxItem(it, byID, now)
		if it.Postponed {
			postponed = append(postponed, en)
			continue
		}
		if it.Kind == inbox.KindNotice {
			updates = append(updates, en)
		} else {
			needsYou = append(needsYou, en)
		}
	}

	counts, err := inbox.CountsByWorkspace(s.DB, nowMs)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	historyCount := 0
	if wsFilter != "" {
		historyCount = counts[wsFilter].History
	} else {
		for _, ws := range scopeList {
			historyCount += counts[ws.ID].History
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"needsYou":     needsYou,
		"postponed":    postponed,
		"updates":      updates,
		"historyCount": historyCount,
	})
}

func (s *Server) apiInboxRespond(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	itemID, err := strconv.ParseInt(r.PathValue("itemId"), 10, 64)
	if err != nil || itemID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_item_id"})
		return
	}
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
		return
	}

	var body struct {
		Decision string `json:"decision"`
		Response string `json:"response"`
		Comment  string `json:"comment"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	now := s.Now()
	nowMs := now.UnixMilli()
	cur, err := inbox.GetInWorkspace(s.DB, ws.ID, itemID, nowMs)
	if err != nil {
		writeInboxError(w, err)
		return
	}

	var updated inbox.Item
	switch cur.Kind {
	case inbox.KindNotice:
		updated, _, err = inbox.Dismiss(s.DB, ws.ID, itemID, nowMs)
	case inbox.KindApprove:
		decision := strings.TrimSpace(body.Decision)
		comment := strings.TrimSpace(body.Comment)
		if decision == "" {
			decision = strings.TrimSpace(body.Response)
		} else if comment == "" {
			comment = strings.TrimSpace(body.Response)
		}
		updated, _, err = inbox.RespondWithComment(s.DB, ws.ID, itemID, decision, comment, nowMs)
	case inbox.KindAgentPrompt:
		decision := strings.TrimSpace(body.Decision)
		if decision == "" {
			decision = strings.TrimSpace(body.Response)
		}
		item, ok := s.answerAgentPromptItem(w, r, ws, itemID, decision)
		if !ok {
			return
		}
		s.notifyStream("inbox")
		writeJSON(w, http.StatusOK, map[string]any{
			"item": s.enrichInboxItem(item, map[string]workspaces.Workspace{ws.ID: ws}, now),
		})
		return
	case inbox.KindChoice:
		val := strings.TrimSpace(body.Decision)
		if val == "" {
			val = strings.TrimSpace(body.Response)
		}
		updated, _, err = inbox.Respond(s.DB, ws.ID, itemID, val, nowMs)
	default:
		updated, _, err = inbox.Respond(s.DB, ws.ID, itemID, body.Response, nowMs)
	}
	if err != nil {
		writeInboxError(w, err)
		return
	}

	s.notifyStream("inbox")
	writeJSON(w, http.StatusOK, map[string]any{
		"item": s.enrichInboxItem(updated, map[string]workspaces.Workspace{ws.ID: ws}, now),
	})
}

func (s *Server) apiInboxDismiss(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	itemID, err := strconv.ParseInt(r.PathValue("itemId"), 10, 64)
	if err != nil || itemID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_item_id"})
		return
	}
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
		return
	}
	now := s.Now()
	updated, _, err := inbox.Dismiss(s.DB, ws.ID, itemID, now.UnixMilli())
	if err != nil {
		writeInboxError(w, err)
		return
	}
	s.notifyStream("inbox")
	writeJSON(w, http.StatusOK, map[string]any{
		"item": s.enrichInboxItem(updated, map[string]workspaces.Workspace{ws.ID: ws}, now),
	})
}

func (s *Server) apiInboxPostpone(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	itemID, err := strconv.ParseInt(r.PathValue("itemId"), 10, 64)
	if err != nil || itemID <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_item_id"})
		return
	}
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
		return
	}

	var body struct {
		Preset  string `json:"preset"`
		UntilMs *int64 `json:"untilMs"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	now := s.Now()
	untilMs, okPreset := resolvePostponeUntil(body.Preset, body.UntilMs, now)
	if !okPreset {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_postpone_target"})
		return
	}

	updated, err := inbox.Postpone(s.DB, ws.ID, itemID, untilMs, now.UnixMilli())
	if err != nil {
		writeInboxError(w, err)
		return
	}
	s.notifyStream("inbox")
	writeJSON(w, http.StatusOK, map[string]any{
		"item": s.enrichInboxItem(updated, map[string]workspaces.Workspace{ws.ID: ws}, now),
	})
}

func resolvePostponeUntil(preset string, explicit *int64, now time.Time) (int64, bool) {
	if explicit != nil {
		return *explicit, true
	}
	local := now.Local()
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case "", "1h", "hour":
		return now.Add(time.Hour).UnixMilli(), true
	case "evening":
		target := time.Date(local.Year(), local.Month(), local.Day(), 18, 0, 0, 0, local.Location())
		if !target.After(local) {
			target = target.AddDate(0, 0, 1)
		}
		return target.UnixMilli(), true
	case "tomorrow":
		tom := local.AddDate(0, 0, 1)
		target := time.Date(tom.Year(), tom.Month(), tom.Day(), 9, 0, 0, 0, local.Location())
		return target.UnixMilli(), true
	case "clear", "now":
		return 0, true
	default:
		return 0, false
	}
}

func (s *Server) apiDismissNotices(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	if wsID != "" {
		list, err := s.mergedWorkspaces()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
			return
		}
		if _, ok := s.findWorkspace(list, wsID); !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
			return
		}
	}
	n, err := inbox.DismissNotices(s.DB, wsID, s.Now().UnixMilli())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	if n > 0 {
		s.notifyStream("inbox")
	}
	writeJSON(w, http.StatusOK, map[string]any{"dismissed": n})
}

func (s *Server) apiChatList(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
		return
	}
	msgs, err := chat.List(s.DB, ws.ID, 0, 200)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": ws,
		"messages":  msgs,
	})
}

func (s *Server) apiChatSend(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	list, err := s.mergedWorkspaces()
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "workspaces_load_failed"})
		return
	}
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace_not_found"})
		return
	}
	var body struct {
		Body string `json:"body"`
		// Thread is the colour the page was showing. Wire can only write to the thread selected
		// in Maestri, so a send is refused when that is no longer the one the person saw.
		Thread string `json:"thread"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	if s.sendWireChat(w, r, ws, body.Body, body.Thread) {
		return
	}
	msg, _, err := chat.Send(s.DB, ws.ID, chat.SenderHuman, body.Body, s.Now().UnixMilli())
	if err != nil {
		if errors.Is(err, chat.ErrInvalidInput) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	s.notifyStream("chat")
	writeJSON(w, http.StatusOK, map[string]any{"message": msg})
}

func (s *Server) evaluateUsageAndStandby() (usage.Snapshot, standby.Summary, bool) {
	now := s.Now()
	cfg := s.configCopy()
	snap := usage.ReadSnapshot(s.DB, cfg, now)
	list, _ := s.mergedWorkspaces()
	tr, err := standby.Evaluate(s.DB, cfg, s.Audit, list, snap, now, nil)
	if err != nil {
		cur, _ := standby.Current(s.DB)
		return snap, cur, false
	}
	s.handleStandbyTransition(tr)
	return snap, tr.Summary, tr.Changed
}

func (s *Server) apiUsage(w http.ResponseWriter, r *http.Request) {
	snap, sb, changed := s.evaluateUsageAndStandby()
	if changed {
		s.notifyStream("standby")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"usage":   snap,
		"standby": sb,
		"config":  s.standbyConfigCopy(),
	})
}

func (s *Server) apiStandbyConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Thresholds         map[string]map[string]int    `json:"thresholds"`
		Actions            map[string]map[string]string `json:"actions"`
		WakeOnReset        *bool                        `json:"wakeOnReset"`
		ResumeBelowPercent *int                         `json:"resumeBelowPercent"`
		ClearResumeBelow   bool                         `json:"clearResumeBelow"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}

	next := s.standbyConfigCopy()
	if body.Thresholds != nil {
		next.Thresholds = body.Thresholds
	}
	if body.Actions != nil {
		next.Actions = body.Actions
	}
	if body.WakeOnReset != nil {
		next.WakeOnReset = *body.WakeOnReset
	}
	if body.ClearResumeBelow {
		next.ResumeBelowPercent = nil
	} else if body.ResumeBelowPercent != nil {
		v := *body.ResumeBelowPercent
		next.ResumeBelowPercent = &v
	}

	s.cfgMu.Lock()
	err := config.SaveStandby(s.Config.ConfigDir, next, s.env)
	if err == nil {
		s.Config.Standby = cloneStandbyConfig(next)
	}
	s.cfgMu.Unlock()
	if err != nil {
		var ce *config.Error
		if errors.As(err, &ce) {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": ce.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "save_standby_failed"})
		return
	}

	snap, sb, _ := s.evaluateUsageAndStandby()
	s.notifyStream("standby")
	writeJSON(w, http.StatusOK, map[string]any{
		"usage":   snap,
		"standby": sb,
		"config":  s.standbyConfigCopy(),
	})
}

func (s *Server) apiStandbyResume(w http.ResponseWriter, r *http.Request) {
	var body struct {
		GuardKey string `json:"guardKey"`
	}
	if err := decodeJSONBody(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_json"})
		return
	}
	list, _ := s.mergedWorkspaces()
	tr, err := standby.ResumeNow(s.DB, s.Audit, list, strings.TrimSpace(body.GuardKey), s.Now(), ipOf(r), nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
		return
	}
	s.notifyStream("standby")
	writeJSON(w, http.StatusOK, map[string]any{
		"standby": tr.Summary,
		"resumed": len(tr.Events),
	})
}

type apiFloorItem struct {
	floors.Floor
	WorkspaceName string `json:"workspace_name,omitempty"`
	WorkspaceTone string `json:"workspace_tone,omitempty"`
}

func (s *Server) apiFloors(w http.ResponseWriter, r *http.Request) {
	wsFilter := strings.TrimSpace(r.URL.Query().Get("workspace"))
	list, err := s.mergedWorkspaces()
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var out []apiFloorItem
	if wsFilter != "" {
		ws, ok := s.findWorkspace(list, wsFilter)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
			return
		}
		items, err := floors.ListWithLive(s.DB, ws.ID, true, s.LiveFloors)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		for _, fl := range items {
			out = append(out, apiFloorItem{
				Floor:         fl,
				WorkspaceName: ws.Name,
				WorkspaceTone: ws.Tone,
			})
		}
	} else {
		for _, ws := range list {
			items, err := floors.ListWithLive(s.DB, ws.ID, false, s.LiveFloors)
			if err != nil {
				s.serverError(w, r, err)
				return
			}
			for _, fl := range items {
				if fl.State != floors.StateActive {
					continue
				}
				out = append(out, apiFloorItem{
					Floor:         fl,
					WorkspaceName: ws.Name,
					WorkspaceTone: ws.Tone,
				})
			}
		}
	}
	if out == nil {
		out = []apiFloorItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"floors": out})
}

func (s *Server) apiWorkspaceFloors(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	list, err := s.mergedWorkspaces()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
		return
	}
	all, err := floors.ListWithLive(s.DB, ws.ID, true, s.LiveFloors)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	present := []floors.Floor{}
	future := []floors.Floor{}
	past := []floors.Floor{}
	for _, fl := range all {
		switch fl.State {
		case floors.StateActive:
			present = append(present, fl)
		case floors.StatePlanned:
			future = append(future, fl)
		case floors.StateLanded, floors.StateCancelled:
			past = append(past, fl)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": ws,
		"present":   present,
		"future":    future,
		"past":      past,
		"floors":    all,
	})
}

func (s *Server) apiWorkspaceDocs(w http.ResponseWriter, r *http.Request) {
	wsID := r.PathValue("id")
	list, err := s.mergedWorkspaces()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	ws, ok := s.findWorkspace(list, wsID)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
		return
	}
	if ws.Path == "" {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace has no local path"})
		return
	}
	res, err := s.docs.View(ws.ID, ws.Path, r.URL.Query().Get("doc"))
	if err != nil {
		switch {
		case errors.Is(err, docs.ErrInvalidPath), errors.Is(err, docs.ErrNotMarkdown):
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		case errors.Is(err, docs.ErrTooLarge):
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
		case errors.Is(err, docs.ErrNotFound):
			writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
		default:
			s.serverError(w, r, err)
		}
		return
	}
	if res.ETag != "" {
		w.Header().Set("ETag", res.ETag)
		if match := strings.TrimSpace(r.Header.Get("If-None-Match")); match == res.ETag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	writeJSON(w, http.StatusOK, res)
}

func writeInboxError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, inbox.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "item_not_found"})
	case errors.Is(err, inbox.ErrNotOpen):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "item_already_closed"})
	case errors.Is(err, inbox.ErrInvalidInput):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal"})
	}
}

func formatRelativeTime(tsMs int64, now time.Time) string {
	if tsMs <= 0 {
		return ""
	}
	t := time.UnixMilli(tsMs)
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		days := int(d.Hours() / 24)
		if days == 1 {
			return "Yesterday"
		}
		if days < 7 {
			return strconv.Itoa(days) + "d"
		}
		return t.Local().Format("Jan 2")
	}
}

func formatWakeTime(untilMs int64, now time.Time) string {
	t := time.UnixMilli(untilMs).Local()
	n := now.Local()
	if t.Year() == n.Year() && t.YearDay() == n.YearDay() {
		return "Back at " + t.Format("15:04")
	}
	return "Back " + t.Format("Jan 2, 15:04")
}
