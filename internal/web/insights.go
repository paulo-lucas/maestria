package web

import (
	"fmt"
	"net/http"
	"time"

	"github.com/paulo-lucas/maestria/internal/insights"
	"github.com/paulo-lucas/maestria/internal/ui"
	"github.com/paulo-lucas/maestria/internal/usage"
)

// insightsCacheFor is how long a computed report is served before it is worked out again.
const insightsCacheFor = time.Minute

func (s *Server) getInsights(w http.ResponseWriter, r *http.Request) {
	d := s.appData(r, "Insights", "insights")
	s.populateShell(d, "all")
	rep, err := s.insightsReport()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Insights = toInsightsView(rep)
	s.render(w, ui.PageInsights, d)
}

func (s *Server) apiInsights(w http.ResponseWriter, r *http.Request) {
	rep, err := s.insightsReport()
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

// insightsReport returns the report, computed at most once a minute.
func (s *Server) insightsReport() (insights.Report, error) {
	s.insightsMu.Lock()
	defer s.insightsMu.Unlock()
	now := s.Now()
	if age := now.Sub(s.insightsAt); !s.insightsAt.IsZero() && age >= 0 && age < insightsCacheFor {
		return s.insightsRep, nil
	}
	rep, err := insights.Compute(s.DB, now)
	if err != nil {
		return rep, err
	}
	s.insightsAt, s.insightsRep = now, rep
	return rep, nil
}

// recordUsage stores a usage sample for Insights. A failure is logged at most once an hour, so
// someone whose page stays on "Collecting usage" can find out why without flooding the log.
func (s *Server) recordUsage(snap usage.Snapshot) {
	err := insights.RecordUsage(s.DB, snap, s.Now())
	if err == nil || s.Log == nil {
		return
	}
	// A negative age (the clock stepped back) logs again rather than muting the warning.
	if age := s.Now().Sub(s.usageErrLogged); !s.usageErrLogged.IsZero() && age >= 0 && age < time.Hour {
		return
	}
	s.usageErrLogged = s.Now()
	fmt.Fprintf(s.Log, "Warning: Insights could not record a usage sample: %v (shown at most once an hour)\n", err)
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
