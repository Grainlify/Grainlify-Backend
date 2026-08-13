package calibration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/jagadeesh/grainlify/backend/internal/db"
)

// The labelling screen, served by the tool itself.
//
// Plain server-rendered HTML from this binary rather than a page in the
// product frontend. Not a shortcut: it means the labelling UI cannot end up in
// a production bundle, cannot import product components, and cannot acquire a
// route in the app by someone wiring it up later. The boundary is that this
// code is somewhere the product does not build from.

// Server holds what the local tool needs to serve one sample.
type Server struct {
	Local      db.DBPool
	SampleName string
}

// Handler builds the HTTP surface. Every response is assembled from the types
// in labelling.go, which name their fields explicitly - so a model output or
// another labeller's verdict cannot reach a page by being added to a table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleIndex)
	mux.HandleFunc("/pr/", s.handlePR)
	mux.HandleFunc("/submit", s.handleSubmit)
	mux.HandleFunc("/agreement", s.handleAgreement)
	return mux
}

func (s *Server) labeller(r *http.Request) (Labeller, error) {
	handle := strings.TrimSpace(r.URL.Query().Get("as"))
	if handle == "" {
		if c, err := r.Cookie("labeller"); err == nil {
			handle = c.Value
		}
	}
	if handle == "" {
		return Labeller{}, ErrNoSuchLabeller
	}
	return LabellerByHandle(r.Context(), s.Local, handle)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// A sample that is not fully snapshotted must not be labellable. Starting
	// on a subset produces a partial set that reads like a whole one.
	cov, err := Coverage(ctx, s.Local, s.SampleName)
	if err != nil {
		httpError(w, err)
		return
	}
	if !cov.Complete {
		renderPage(w, pageData{
			Title:   "Not ready",
			Blocked: fmt.Sprintf("Sample %q is %d of %d snapshotted. Run `calibrate -name %s -snapshot` before labelling.", s.SampleName, cov.Snapshot, cov.Total, s.SampleName),
		})
		return
	}

	lab, err := s.labeller(r)
	if err != nil {
		s.renderChooseLabeller(w, r)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "labeller", Value: lab.Handle, Path: "/"})

	queue, err := Queue(ctx, s.Local, s.SampleName, lab.ID)
	if err != nil {
		httpError(w, err)
		return
	}

	done := 0
	for _, q := range queue {
		if q.Labelled {
			done++
		}
	}
	renderPage(w, pageData{
		Title:    "Queue",
		Labeller: lab,
		Queue:    queue,
		Done:     done,
		Total:    len(queue),
	})
}

func (s *Server) renderChooseLabeller(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Local.Query(r.Context(), `SELECT handle, display_name FROM calibration_labellers WHERE retired_at IS NULL ORDER BY handle`)
	if err != nil {
		httpError(w, err)
		return
	}
	defer rows.Close()
	var labs []Labeller
	for rows.Next() {
		var l Labeller
		if err := rows.Scan(&l.Handle, &l.DisplayName); err == nil {
			labs = append(labs, l)
		}
	}
	renderPage(w, pageData{Title: "Who is labelling?", Labellers: labs})
}

func (s *Server) handlePR(w http.ResponseWriter, r *http.Request) {
	lab, err := s.labeller(r)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/pr/")
	id, err := uuid.Parse(idStr)
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}

	pr, err := GetPRForLabelling(r.Context(), s.Local, s.SampleName, id, lab.ID)
	if errors.Is(err, ErrNotInSample) {
		http.Error(w, "not in this sample, or held back", http.StatusNotFound)
		return
	}
	if err != nil {
		httpError(w, err)
		return
	}

	queue, _ := Queue(r.Context(), s.Local, s.SampleName, lab.ID)
	pos, next := 0, ""
	for i, q := range queue {
		if q.SamplePRID == id {
			pos = i + 1
			for _, later := range queue[i+1:] {
				if !later.Labelled {
					next = later.SamplePRID.String()
					break
				}
			}
		}
	}
	if next == "" {
		for _, q := range queue {
			if !q.Labelled && q.SamplePRID != id {
				next = q.SamplePRID.String()
				break
			}
		}
	}

	renderPage(w, pageData{
		Title:    fmt.Sprintf("%s#%d", pr.Project, pr.Number),
		Labeller: lab,
		PR:       &pr,
		Position: pos,
		Total:    len(queue),
		NextID:   next,
	})
}

func (s *Server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	lab, err := s.labeller(r)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	id, err := uuid.Parse(r.FormValue("sample_pr_id"))
	if err != nil {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	reason := strings.TrimSpace(r.FormValue("reason"))
	if len(reason) < 10 {
		http.Error(w, "a reason of at least 10 characters is required: when the model disagrees, the reason is the only thing that says which of you was wrong", http.StatusBadRequest)
		return
	}

	if _, err := SubmitLabel(r.Context(), s.Local, id, lab.ID,
		r.FormValue("verdict"), reason, r.FormValue("confidence")); err != nil {
		httpError(w, err)
		return
	}
	if next := r.FormValue("next"); next != "" {
		http.Redirect(w, r, "/pr/"+next, http.StatusFound)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleAgreement(w http.ResponseWriter, r *http.Request) {
	lab, err := s.labeller(r)
	if err != nil {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	rows, err := Agreement(r.Context(), s.Local, s.SampleName, lab.ID)
	if err != nil {
		httpError(w, err)
		return
	}
	agree := 0
	for _, row := range rows {
		if row.Agree {
			agree++
		}
	}
	renderPage(w, pageData{Title: "Agreement", Labeller: lab, Agreement: rows, AgreeCount: agree})
}

func httpError(w http.ResponseWriter, err error) {
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

type pageData struct {
	Title      string
	Blocked    string
	Labeller   Labeller
	Labellers  []Labeller
	Queue      []QueueItem
	PR         *PRForLabelling
	Position   int
	Total      int
	Done       int
	NextID     string
	Agreement  []AgreementRow
	AgreeCount int
}

func renderPage(w http.ResponseWriter, d pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := pageTmpl.Execute(w, d); err != nil {
		fmt.Fprintf(w, "<pre>template error: %v</pre>", err)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

var pageTmpl = template.Must(template.New("page").Funcs(template.FuncMap{
	"json": mustJSON,
}).Parse(`<!doctype html>
<html><head><meta charset="utf-8"><title>{{.Title}} - calibration</title>
<style>
 body{font:14px/1.55 -apple-system,BlinkMacSystemFont,"Segoe UI",sans-serif;margin:0;background:#f6f6f4;color:#1c1c1a}
 header{background:#221f1b;color:#e8dfd0;padding:10px 20px;display:flex;gap:16px;align-items:baseline}
 header a{color:#e8c571;text-decoration:none} header .who{margin-left:auto;opacity:.75}
 main{max-width:1100px;margin:0 auto;padding:20px}
 .card{background:#fff;border:1px solid #e2ded6;border-radius:10px;padding:16px;margin-bottom:16px}
 .muted{color:#77706a} .warn{background:#fff6e5;border-color:#e8c571}
 h1{font-size:19px;margin:0 0 4px} h2{font-size:14px;margin:0 0 8px;text-transform:uppercase;letter-spacing:.04em;color:#77706a}
 pre{background:#1c1c1a;color:#e6e6e6;padding:12px;border-radius:8px;overflow:auto;max-height:520px;font-size:12px}
 ul.q{list-style:none;padding:0;margin:0} ul.q li{padding:7px 0;border-bottom:1px solid #eee}
 .done{color:#3a7d44} .files{font-family:ui-monospace,monospace;font-size:12px}
 button{font:inherit;padding:8px 16px;border-radius:8px;border:1px solid #ccc;background:#fff;cursor:pointer}
 button.acc{background:#e7f4e9;border-color:#3a7d44;color:#2b5c33} button.rej{background:#fdecec;border-color:#b03535;color:#8c2a2a}
 textarea{width:100%;font:inherit;padding:8px;border-radius:8px;border:1px solid #ccc;box-sizing:border-box}
 label.r{margin-right:14px}
</style></head><body>
<header>
  <strong>calibration</strong>
  <a href="/">queue</a><a href="/agreement">agreement</a>
  {{if .Labeller.Handle}}<span class="who">labelling as {{.Labeller.DisplayName}} ({{.Labeller.Handle}})</span>{{end}}
</header>
<main>

{{if .Blocked}}<div class="card warn"><h1>Not ready</h1><p>{{.Blocked}}</p></div>{{end}}

{{if .Labellers}}
  <div class="card"><h1>Who is labelling?</h1>
  <p class="muted">Labelling identity is separate from any product account or role.</p>
  <ul class="q">{{range .Labellers}}<li><a href="/?as={{.Handle}}">{{.DisplayName}} <span class="muted">({{.Handle}})</span></a></li>{{end}}</ul></div>
{{end}}

{{if .Queue}}
  <div class="card"><h1>Queue</h1>
    <p class="muted">{{.Done}} of {{.Total}} labelled by you. Held-back pull requests are not shown.</p>
    <ul class="q">{{range .Queue}}
      <li><a href="/pr/{{.SamplePRID}}">{{.Project}}#{{.Number}}</a>
      {{if .Labelled}}<span class="done"> - you labelled this</span>{{end}}</li>
    {{end}}</ul>
  </div>
{{end}}

{{with .PR}}
  <div class="card">
    <h1>{{.Project}}#{{.Number}} - {{.Title}}</h1>
    <p class="muted">{{$.Position}} of {{$.Total}} &middot; by {{.Author}} &middot;
      +{{.Additions}} / -{{.Deletions}} across {{.ChangedFiles}} files &middot;
      <a href="{{.URL}}" target="_blank" rel="noopener">open on GitHub</a></p>
  </div>

  <div class="card"><h2>Description</h2>
    {{if .Body}}<pre style="background:#faf9f7;color:#1c1c1a;max-height:240px">{{.Body}}</pre>
    {{else}}<p class="muted">This pull request has no description.</p>{{end}}
  </div>

  <div class="card {{if not .HasIssue}}warn{{end}}"><h2>Linked issue</h2>
    {{if .HasIssue}}
      <p><strong>#{{.IssueNumber}} {{.IssueTitle}}</strong></p>
      <pre style="background:#faf9f7;color:#1c1c1a;max-height:300px">{{.IssueBody}}</pre>
    {{else}}
      <p><strong>No issue linked.</strong> This pull request does not reference an
      issue, so there are no acceptance criteria to judge it against. That is a
      fact about the pull request, not a page that failed to load - judge it on
      its own terms.</p>
    {{end}}
  </div>

  <div class="card"><h2>Files</h2>
    <div class="files">{{range .Files}}{{.Filename}} <span class="muted">({{.Status}}, +{{.Additions}} -{{.Deletions}})</span><br>{{end}}</div>
  </div>

  <div class="card"><h2>Diff</h2>
    {{if .DiffTruncated}}<p class="muted"><strong>This diff is truncated.</strong> You are not seeing the whole change.</p>{{end}}
    <pre>{{.Diff}}</pre>
  </div>

  <div class="card"><h2>Your verdict</h2>
    {{if .MyLabel}}<p class="muted">You previously said <strong>{{.MyLabel.Verdict}}</strong> ({{.MyLabel.Confidence}}): {{.MyLabel.Reason}}<br>
    Submitting again records a new label; the original is kept.</p>{{end}}
    <form method="post" action="/submit">
      <input type="hidden" name="sample_pr_id" value="{{.SamplePRID}}">
      <input type="hidden" name="next" value="{{$.NextID}}">
      <p><textarea name="reason" rows="3" required minlength="10"
         placeholder="Why? Required, and the only thing that will explain a disagreement later.">{{if .MyLabel}}{{.MyLabel.Reason}}{{end}}</textarea></p>
      <p>
        <label class="r"><input type="radio" name="confidence" value="certain" required> Certain</label>
        <label class="r"><input type="radio" name="confidence" value="borderline"> Borderline</label>
      </p>
      <p>
        <button class="acc" name="verdict" value="accept" type="submit">Accept</button>
        <button class="rej" name="verdict" value="reject" type="submit">Reject</button>
      </p>
    </form>
  </div>
{{end}}

{{if .Agreement}}
  <div class="card"><h1>Agreement</h1>
    <p class="muted">{{.AgreeCount}} of {{len .Agreement}} pull requests agree. Only pull
    requests you and another labeller have both submitted on appear here.</p>
    <ul class="q">{{range .Agreement}}
      <li>{{.Project}}#{{.Number}} - {{json .Verdicts}} {{if .Agree}}<span class="done">agree</span>{{else}}<strong>disagree</strong>{{end}}</li>
    {{end}}</ul>
  </div>
{{else}}{{if eq .Title "Agreement"}}
  <div class="card"><h1>Agreement</h1><p class="muted">Nothing to compare yet. A pull
  request appears here once you and another labeller have both submitted on it -
  never before, because seeing another verdict first would make the number
  measure influence rather than agreement.</p></div>
{{end}}{{end}}

</main></body></html>`))

// Serve runs the local labelling server.
func Serve(ctx context.Context, local db.DBPool, sampleName, addr string) error {
	s := &Server{Local: local, SampleName: sampleName}
	srv := &http.Server{Addr: addr, Handler: s.Handler()}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	return srv.ListenAndServe()
}
