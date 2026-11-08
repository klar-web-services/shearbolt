package plan

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// ParseAnswers parses --answer arguments of the form id=value. IDs never contain "=", so the
// first "=" separates the ID from the value.
func ParseAnswers(args []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range args {
		id, value, ok := strings.Cut(a, "=")
		if !ok || id == "" {
			return nil, fmt.Errorf("answer %q must have the form id=value", a)
		}
		if _, dup := out[id]; dup {
			return nil, fmt.Errorf("question %s is answered twice", id)
		}
		out[id] = value
	}
	return out, nil
}

// Resolve checks the answers against the questions and returns the value of every applicable
// question: its answer, or its default. missing lists the applicable questions without an
// answer, in question order; it is empty when yes is set.
//
// A question applies when its Project (if set) is selected and none of its UnlessChosen
// questions resolves to that Project. The selected projects come from the "projects" question,
// or are all projects when there is none. Inapplicable questions get no value; answers to them
// are checked but otherwise ignored.
func Resolve(qs []Question, answers map[string]string, yes bool) (values map[string]string, missing []string, err error) {
	byID := map[string]int{}
	for i, q := range qs {
		byID[q.ID] = i
	}
	for _, id := range slices.Sorted(maps.Keys(answers)) {
		if _, ok := byID[id]; !ok {
			return nil, nil, fmt.Errorf("unknown question %s; the questions are: %s", id, strings.Join(ids(qs), ", "))
		}
	}
	given := map[string]string{}
	for _, q := range qs {
		a, ok := answers[q.ID]
		if !ok {
			continue
		}
		v, err := normalize(q, a)
		if err != nil {
			return nil, nil, err
		}
		given[q.ID] = v
	}
	value := func(q Question) string {
		if v, ok := given[q.ID]; ok {
			return v
		}
		return q.Default
	}
	selected := func(string) bool { return true }
	if i, ok := byID["projects"]; ok {
		chosen := splitValues(value(qs[i]))
		selected = func(dir string) bool { return slices.Contains(chosen, dir) }
	}

	memo := map[string]bool{}
	visiting := map[string]bool{}
	var applies func(i int) bool
	applies = func(i int) bool {
		q := qs[i]
		if v, ok := memo[q.ID]; ok {
			return v
		}
		if visiting[q.ID] {
			return true // a cycle: ask rather than guess
		}
		visiting[q.ID] = true
		ok := q.Project == "" || selected(q.Project)
		for _, id := range q.UnlessChosen {
			j, found := byID[id]
			if ok && found && applies(j) && resolvesTo(qs[j], value(qs[j]), q.Project) {
				ok = false
			}
		}
		delete(visiting, q.ID)
		memo[q.ID] = ok
		return ok
	}

	values = map[string]string{}
	for i, q := range qs {
		if !applies(i) {
			continue
		}
		values[q.ID] = value(q)
		if _, ok := given[q.ID]; !ok && !yes {
			missing = append(missing, q.ID)
		}
	}
	return values, missing, nil
}

// normalize validates answer a to q. A multi answer is returned deduplicated, in option order.
func normalize(q Question, a string) (string, error) {
	valid := make([]string, len(q.Options))
	for i, o := range q.Options {
		valid[i] = o.Value
	}
	if q.Kind != "multi" {
		if !slices.Contains(valid, a) {
			return "", fmt.Errorf("invalid answer %q for %s; valid values: %s", a, q.ID, strings.Join(valid, ", "))
		}
		return a, nil
	}
	if strings.TrimSpace(a) == "" {
		return "", fmt.Errorf("the answer for %s is empty; give one or more of: %s", q.ID, strings.Join(valid, ", "))
	}
	chosen := map[string]bool{}
	for _, v := range strings.Split(a, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			return "", fmt.Errorf("the answer %q for %s has an empty value; valid values: %s", a, q.ID, strings.Join(valid, ", "))
		}
		if !slices.Contains(valid, v) {
			return "", fmt.Errorf("invalid value %q for %s; valid values: %s", v, q.ID, strings.Join(valid, ", "))
		}
		chosen[v] = true
	}
	var out []string
	for _, v := range valid {
		if chosen[v] {
			out = append(out, v)
		}
	}
	return strings.Join(out, ","), nil
}

func resolvesTo(q Question, value, project string) bool {
	if project == "" {
		return false
	}
	if q.Kind == "multi" {
		return slices.Contains(splitValues(value), project)
	}
	return value == project
}

func splitValues(v string) []string {
	var out []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func ids(qs []Question) []string {
	out := make([]string, len(qs))
	for i, q := range qs {
		out[i] = q.ID
	}
	return out
}
