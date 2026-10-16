// SPDX-License-Identifier: AGPL-3.0-or-later

package usecase

import (
	"strings"

	"bnema.dev/ermite-actions/template"
	"bnema.dev/ermite-actions/workflow"
	"bnema.dev/ermite/internal/domain"
)

// parseWorkflow is workflow.Parse plus the checks GitHub makes when it reads a
// workflow that the parser of ermite-actions does not: a schedule timezone that
// is no IANA time zone name, and queue: max with cancel-in-progress: true (CI-4
// D6). A workflow with such an error is invalid like one that does not parse.
func parseWorkflow(path string, content []byte) (*workflow.Workflow, []workflow.Error) {
	wf, errs := workflow.Parse(path, content)
	if len(errs) > 0 || wf == nil {
		return wf, errs
	}
	return wf, validateWorkflow(path, wf)
}

func validateWorkflow(path string, wf *workflow.Workflow) []workflow.Error {
	var errs []workflow.Error
	if wf.On != nil {
		if ev := wf.On.Event(eventSchedule); ev != nil {
			for _, sc := range ev.Schedule {
				if _, err := domain.ParseCronTimezone(sc.Timezone); err != nil {
					errs = append(errs, workflow.Error{File: path, Raw: strings.TrimPrefix(err.Error(), domain.ErrInvalid.Error()+": ")})
				}
			}
		}
	}
	check := func(tok template.Token) {
		if pos, bad := queueMaxWithCancel(tok); bad {
			errs = append(errs, workflow.Error{File: path, Pos: pos, Raw: errQueueMaxCancel.Error()})
		}
	}
	check(wf.Concurrency)
	for _, j := range wf.Jobs {
		check(j.Concurrency)
	}
	return errs
}

// queueMaxWithCancel reports a concurrency mapping that writes queue: max and
// cancel-in-progress: true as literals, the pair GitHub refuses as a workflow
// validation error (docs.github.com, "Control the concurrency of workflows and
// jobs": "The combination of `queue: max` and `cancel-in-progress: true` is not
// allowed and will result in a workflow validation error"; languageservices
// languageservice/src/validate.ts:702-743 checks the literal pair). An
// expression is checked when it is evaluated (evalConcurrency).
func queueMaxWithCancel(tok template.Token) (template.Pos, bool) {
	m, ok := tok.(*template.Mapping)
	if !ok {
		return template.Pos{}, false
	}
	q, ok := m.Find("queue").(*template.String)
	if !ok || q.Value != "max" {
		return template.Pos{}, false
	}
	c, ok := m.Find("cancel-in-progress").(*template.Boolean)
	if !ok || !c.Value {
		return template.Pos{}, false
	}
	return c.Pos(), true
}
