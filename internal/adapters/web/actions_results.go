// SPDX-License-Identifier: AGPL-3.0-or-later

package web

import (
	"context"
	"html/template"
	"path"
	"strconv"
	"strings"

	"bnema.dev/ermite/internal/domain"
	"bnema.dev/ermite/internal/ports"
)

// The results of a job on the CI pages: the annotations (the "Annotations" box
// of the run page, and at their step on the job page) and the job summaries
// under the run. Both are untrusted text of the workflow: annotations are
// escaped by the templates, summaries go through the Markdown renderer of the
// READMEs (raw HTML dropped, URLs filtered, images shown as their alt text).

// maxSummaryPageBytes bounds the Markdown rendered in one page, all summaries
// together; the ones past it are not shown, with a note.
const maxSummaryPageBytes = 4 << 20

// AnnotationView is an annotation as shown.
type AnnotationView struct {
	// Kind is the colour class (notice, warning, failure) and Level its word.
	Kind, Level  string
	Job, JobHref string
	// Step is the name of the step and StepHref jumps to its lines in the log
	// of the job.
	Step, StepHref string
	Title, Message string
	// Where is "file:line", WhereHref the file at the commit of the run, "" when
	// the path is not one of the repository.
	Where, WhereHref string
	// StepHead is set on the first annotation of a step in a list grouped by
	// step (the job page).
	StepHead bool
}

// stepInfo is what the views need of the step an annotation belongs to.
type stepInfo struct {
	name       string
	start, end int64
}

// jobInfo is what the annotation views need of a job.
type jobInfo struct {
	name, href string
	steps      map[int32]stepInfo
}

func jobInfoOf(repo string, runID int64, j ports.JobView, linkJob bool) jobInfo {
	info := jobInfo{name: visibleText(j.Name), href: jobHref(repo, runID, j.ID), steps: map[int32]stepInfo{}}
	if !linkJob {
		info.href = ""
	}
	for i, st := range j.Steps {
		r := j.StepRanges()[i]
		info.steps[st.Number] = stepInfo{name: visibleText(st.Name), start: r.Start, end: r.End}
	}
	return info
}

func annotationKind(l domain.AnnotationLevel) (kind, word string) {
	switch l {
	case domain.AnnotationFailure:
		return "failure", "error"
	case domain.AnnotationWarning:
		return "warning", "warning"
	}
	return "notice", "notice"
}

// annotationWhere is the "file:line" of an annotation, and a link to the file
// at the commit when the path is a path of the repository.
func annotationWhere(repo, sha string, a domain.CIAnnotation) (text, href string) {
	if a.File == "" {
		return "", ""
	}
	text = visibleText(a.File)
	if a.StartLine > 0 {
		text += ":" + strconv.Itoa(int(a.StartLine))
		if a.EndLine > a.StartLine {
			text += "-" + strconv.Itoa(int(a.EndLine))
		}
	}
	if !repoPath(a.File) {
		return text, ""
	}
	href = repoHref(repo, "blob", sha, a.File)
	if a.StartLine > 0 {
		href += "#L" + strconv.Itoa(int(a.StartLine))
		if a.EndLine > a.StartLine {
			href += "-L" + strconv.Itoa(int(a.EndLine))
		}
	}
	return text, href
}

// repoPath reports whether a path of an annotation is a clean path inside the
// repository: relative, without "." or ".." segments, backslashes or control
// characters.
func repoPath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.ContainsAny(p, "\\\x00\r\n\t") || path.Clean(p) != p {
		return false
	}
	return !strings.HasPrefix(p, "../") && p != ".." && p != "."
}

// annotationViews builds the views of annotations, in their order. jobs maps a
// job id to what is known of it; an annotation of an unknown job is skipped.
func annotationViews(repo, sha string, list ports.AnnotationList, jobs map[string]jobInfo) []AnnotationView {
	out := make([]AnnotationView, 0, len(list.Annotations))
	for _, ja := range list.Annotations {
		job, ok := jobs[ja.JobID]
		if !ok {
			continue
		}
		a := ja.Annotation
		v := AnnotationView{
			Job: job.name, JobHref: job.href, Title: visibleText(a.Title), Message: visibleText(a.Message),
		}
		v.Kind, v.Level = annotationKind(a.Level)
		if st, ok := job.steps[a.Step]; ok {
			v.Step = st.name
			if st.end > st.start && job.href != "" {
				v.StepHref = job.href + "#L" + strconv.FormatInt(st.start, 10)
			}
		}
		if v.Step == "" {
			v.Step = "step " + strconv.Itoa(int(a.Step))
		}
		v.Where, v.WhereHref = annotationWhere(repo, sha, a)
		out = append(out, v)
	}
	return out
}

// SummaryView is the summary of one step, rendered.
type SummaryView struct {
	Step string
	HTML template.HTML
	// Note replaces the content when it is not shown.
	Note string
}

// JobSummaries are the summaries of one job.
type JobSummaries struct {
	Job, JobHref string
	Steps        []SummaryView
	// More says that the job has more summaries than the 20 shown.
	More bool
}

// jobSummaries reads and renders the summaries of the jobs of a run, in the
// order of the jobs, within maxSummaryPageBytes of Markdown.
func (s *Server) jobSummaries(ctx context.Context, d *repoData, run domain.CIRun, jobs []ports.JobView, linkJobs bool) ([]JobSummaries, error) {
	budget := maxSummaryPageBytes
	var out []JobSummaries
	for _, j := range jobs {
		if j.Phase != domain.CIPhaseRunning && !j.Ended() {
			continue // no step has run
		}
		res, err := s.cfg.Runs.GetStepSummaries(ctx, principal(ctx), d.repo.Name, j.ID, j.Attempt)
		if err != nil {
			return nil, err
		}
		if len(res.Summaries) == 0 {
			continue
		}
		info := jobInfoOf(d.name(), run.ID, j, linkJobs)
		js := JobSummaries{Job: info.name, JobHref: info.href, More: res.Truncated}
		for _, sm := range res.Summaries {
			name := "step " + strconv.Itoa(int(sm.Step))
			if st, ok := info.steps[sm.Step]; ok && st.name != "" {
				name = st.name
			}
			v := SummaryView{Step: name}
			if len(sm.Markdown) > budget {
				v.Note = "this summary is not shown: the summaries of the page are over " + humanSize(maxSummaryPageBytes) + "."
				js.Steps = append(js.Steps, v)
				continue
			}
			budget -= len(sm.Markdown)
			prefix := headingIDPrefix + "s" + j.ID + "-" + strconv.Itoa(int(sm.Step)) + "-"
			h, _, err := s.md.renderWithIDs([]byte(sm.Markdown), d.name(), run.Sha, "", prefix)
			if err != nil {
				v.Note = "this summary cannot be shown."
			} else {
				v.HTML = h
			}
			js.Steps = append(js.Steps, v)
		}
		out = append(out, js)
	}
	return out, nil
}

// runAnnotations reads the annotations of a run attempt and builds their views.
func (s *Server) runAnnotations(ctx context.Context, d *repoData, run domain.CIRun, attempt int32, jobs []ports.JobView, linkJobs bool) (annotations []AnnotationView, truncated bool, err error) {
	list, err := s.cfg.Runs.ListAnnotations(ctx, principal(ctx), d.repo.Name, run.ID, attempt, "")
	if err != nil {
		return nil, false, err
	}
	infos := make(map[string]jobInfo, len(jobs))
	for _, j := range jobs {
		infos[j.ID] = jobInfoOf(d.name(), run.ID, j, linkJobs)
	}
	return annotationViews(d.name(), run.Sha, list, infos), list.Truncated, nil
}

// jobAnnotations reads the annotations of one job and builds their views,
// grouped by step.
func (s *Server) jobAnnotations(ctx context.Context, d *repoData, runID int64, sha string, view ports.JobView, attempt int32) ([]AnnotationView, bool, error) {
	list, err := s.cfg.Runs.ListAnnotations(ctx, principal(ctx), d.repo.Name, runID, attempt, view.ID)
	if err != nil {
		return nil, false, err
	}
	views := annotationViews(d.name(), sha, list, map[string]jobInfo{view.ID: jobInfoOf(d.name(), runID, view, true)})
	for i := range views {
		views[i].StepHead = i == 0 || views[i-1].Step != views[i].Step
	}
	return views, list.Truncated, nil
}
