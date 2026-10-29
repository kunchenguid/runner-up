package plan

import (
	"fmt"
	"path"
	"strings"
	"time"
)

// The order rules decide which waiting job a free place takes. Each rule
// splits the line in two, matching jobs first; the next rule orders each half,
// and first-in, first-out breaks the last tie:
//
//	label:NAME        jobs from an open pull request labeled NAME, from the
//	                  same repository, including runs on its head branch
//	waited:DURATION   jobs waiting at least DURATION, oldest first among them,
//	                  ahead of everything the later rules would prefer
//	workflow:GLOB,..  jobs whose top-level workflow file, without .yml or
//	                  .yaml, matches a glob, such as release or release-*
//	shortest          shorter learned runtimes first; a job never seen to
//	                  succeed counts as unknownDuration
//
// GitHub picks which of a scale set's assigned jobs a new runner takes, so the
// order is exact between pools and repositories but only approximate inside
// one scale set.
var defaultOrder = []string{"waited:60m", "shortest"}

const unknownDuration = 15 * time.Minute

type Rule struct {
	kind  string
	label string
	wait  time.Duration
	globs []string
}

func ParseOrder(order []string) ([]Rule, error) {
	if len(order) == 0 {
		order = defaultOrder
	}
	var out []Rule
	for _, s := range order {
		kind, arg, _ := strings.Cut(s, ":")
		r := Rule{kind: kind}
		switch kind {
		case "label":
			if arg == "" {
				return nil, fmt.Errorf("order rule %q names no label", s)
			}
			r.label = arg
		case "waited":
			d, err := time.ParseDuration(arg)
			if err != nil || d <= 0 {
				return nil, fmt.Errorf("order rule %q needs a duration, such as waited:60m", s)
			}
			r.wait = d
		case "workflow":
			for _, g := range strings.Split(arg, ",") {
				g = strings.TrimSpace(g)
				if _, err := path.Match(g, ""); err != nil || g == "" {
					return nil, fmt.Errorf("order rule %q has a bad glob %q", s, g)
				}
				r.globs = append(r.globs, g)
			}
		case "shortest":
			if arg != "" {
				return nil, fmt.Errorf("order rule %q takes no argument", s)
			}
		default:
			return nil, fmt.Errorf("unknown order rule %q: use label:, waited:, workflow: or shortest", s)
		}
		out = append(out, r)
	}
	return out, nil
}

// before reports whether job x goes ahead of job y.
func before(x, y Job, rules []Rule, now time.Time) bool {
	for _, r := range rules {
		switch r.kind {
		case "label":
			if a, b := x.Labels[r.label], y.Labels[r.label]; a != b {
				return a
			}
		case "waited":
			a := !x.QueueTime.IsZero() && now.Sub(x.QueueTime) >= r.wait
			b := !y.QueueTime.IsZero() && now.Sub(y.QueueTime) >= r.wait
			if a != b {
				return a
			}
			if a {
				return fifo(x, y)
			}
		case "workflow":
			if a, b := r.matches(x), r.matches(y); a != b {
				return a
			}
		case "shortest":
			dx, dy := x.Estimate, y.Estimate
			if dx <= 0 {
				dx = unknownDuration
			}
			if dy <= 0 {
				dy = unknownDuration
			}
			if dx != dy {
				return dx < dy
			}
		}
	}
	return fifo(x, y)
}

// matches reads the run's top-level workflow file, falling back to the job's
// own when the run's could not be read. A display name or branch never
// counts, nor does a workflow in another repository's folder.
func (r Rule) matches(j Job) bool {
	w := j.RunWorkflow
	if w == "" {
		w = j.Workflow
	}
	name := path.Base(WorkflowPath(w))
	if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
		return false
	}
	name = strings.TrimSuffix(strings.TrimSuffix(name, ".yml"), ".yaml")
	for _, g := range r.globs {
		if ok, _ := path.Match(g, name); ok {
			return true
		}
	}
	return false
}

// Needs reports which readers the rules use: pull request labels, and the
// run's top-level workflow.
func Needs(rules []Rule) (labels, workflows bool) {
	for _, r := range rules {
		labels = labels || r.kind == "label"
		workflows = workflows || r.kind == "workflow"
	}
	return
}

// workflowRef is the git ref of a job's own repository workflow, or "" for a
// workflow in another repository, so a reusable workflow elsewhere cannot
// claim a label.
func (j Job) workflowRef() string {
	if !strings.HasPrefix(j.Workflow, j.Repo+"/.github/workflows/") {
		return ""
	}
	_, ref, _ := strings.Cut(j.Workflow, "@")
	return ref
}

// LabelRef is the ref a pull request label may reach: a pull request's merge
// or head ref, or a branch. A branch named after a label grants nothing unless
// the reader found that labeled pull request's head there.
func (j Job) LabelRef() string {
	ref := j.workflowRef()
	if p, ok := strings.CutPrefix(ref, "refs/pull/"); ok {
		n, tail, ok := strings.Cut(p, "/")
		if ok && (tail == "merge" || tail == "head") && n != "" && strings.Trim(n, "0123456789") == "" {
			return ref
		}
		return ""
	}
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok && b != "" {
		return ref
	}
	return ""
}
