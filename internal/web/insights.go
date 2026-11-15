package web

import (
	"net/http"
	"time"

	"github.com/paulo-lucas/maestria/internal/insights"
	"github.com/paulo-lucas/maestria/internal/ui"
)

func (s *Server) getInsights(w http.ResponseWriter, r *http.Request) {
	d := s.appData(r, "Insights", "insights")
	s.populateShell(d, "all")
	rep, err := insights.Compute(s.DB, s.Now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Insights = toInsightsView(rep)
	s.render(w, ui.PageInsights, d)
}

func (s *Server) apiInsights(w http.ResponseWriter, r *http.Request) {
	rep, err := insights.Compute(s.DB, s.Now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func toInsightsView(rep insights.Report) *ui.InsightsView {
	day := func(ms int64) string { return time.UnixMilli(ms).Format("Jan 2") }
	v := &ui.InsightsView{Level: rep.Level, Period: day(rep.From) + " – " + day(rep.To)}
	for _, a := range rep.Areas {
		focus := a.ID == rep.Focus
		if focus {
			v.FocusName = a.Title
		}
		v.Areas = append(v.Areas, ui.InsightAreaView{
			ID: a.ID, Title: a.Title, Level: a.Level, Value: a.Value, Next: a.Next,
			Tip: a.Tip, Note: a.Note, Missing: a.Missing, Focus: focus,
		})
	}
	return v
}
