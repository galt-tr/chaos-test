package logs

import (
	"fmt"
	"regexp"
	"strings"
)

// Filter selects lines. The zero value matches everything.
type Filter struct {
	MinLevel string   // "" or ALL = no level floor
	Include  []string // service tags to keep; empty = all
	Exclude  []string // service tags to drop; applied after Include
	Grep     string   // substring, or the source of Re
	Re       *regexp.Regexp
	Case     bool // case-sensitive substring match
}

// ParseFilter builds a Filter from request parameters. A service list may use a leading
// "!" to exclude, e.g. "bval,p2p" or "!alert,!asset".
func ParseFilter(level, services, grep string, useRegex, caseSensitive bool) (Filter, error) {
	f := Filter{Case: caseSensitive, Grep: grep}
	level = strings.ToUpper(strings.TrimSpace(level))
	if level != "" && level != "ALL" {
		if _, ok := levelRank[level]; !ok {
			return f, fmt.Errorf("unknown level %q (want one of %s or ALL)", level, strings.Join(Levels, ", "))
		}
		f.MinLevel = level
	}
	for _, s := range strings.Split(services, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		if strings.HasPrefix(s, "!") {
			f.Exclude = append(f.Exclude, strings.TrimPrefix(s, "!"))
		} else {
			f.Include = append(f.Include, s)
		}
	}
	if grep != "" && useRegex {
		// Go's regexp is RE2: no lookaround, no backreferences. Reject rather than
		// silently behaving differently from what the user typed in the browser.
		re, err := regexp.Compile(grep)
		if err != nil {
			return f, fmt.Errorf("bad regex: %w", err)
		}
		f.Re = re
	}
	return f, nil
}

// Match reports whether a line survives the filter.
//
// Two deliberate exceptions keep the viewer trustworthy:
//   - a line we could not parse (a panic, a runtime's own stderr) is ALWAYS kept, because
//     unparsed lines are rare and uniformly high-signal;
//   - a line's continuations are matched as part of it, so grepping for a root cause finds
//     the parent entry rather than nothing.
func (f Filter) Match(l *Line) bool {
	if l.Level == "" {
		return true
	}
	if f.MinLevel != "" && Rank(l.Level) < Rank(f.MinLevel) {
		return false
	}
	if len(f.Include) > 0 && !contains(f.Include, l.Service) {
		return false
	}
	if contains(f.Exclude, l.Service) {
		return false
	}
	if f.Grep == "" {
		return true
	}
	hay := l.joined()
	if f.Re != nil {
		return f.Re.MatchString(hay)
	}
	if f.Case {
		return strings.Contains(hay, f.Grep)
	}
	return strings.Contains(strings.ToLower(hay), strings.ToLower(f.Grep))
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
