package config

import (
	"fmt"
	"net/url"
	"path"
	"strconv"
	"strings"
)

type project struct {
	ServerURL string `yaml:"url"`
	Name      string `yaml:"project"`
	Token     string
	// Optional list of specific issues to move
	Issues []string
	// Same as Issues but converted to int by Parse
	issues []issueRange
	// If true, ignore source issues and copy labels only
	LabelsOnly bool `yaml:"labelsOnly"`
	// If true, ignore source issues and copy milestones only
	MilestonesOnly bool `yaml:"milestonesOnly"`
	// If true, move the issues (delete theme from the source project)
	MoveIssues bool `yaml:"moveIssues"`
	// Optional user tokens to write notes preserving ownership
	Users map[string]string `yaml:"users"`
	// If true, auto close source issue
	AutoCloseIssues bool `yaml:"autoCloseIssues"`
	// If true, add a link to target issue
	LinkToTargetIssue bool `yaml:"linkToTargetIssue"`
	// Optional caption to use for the link text
	LinkToTargetIssueText string `yaml:"linkToTargetIssueText"`
}

// matches checks whether issue is part of p.issues. Always
// true if p.issues is an empty list, otherwise check all entries
// and ranges, if any.
func (p *project) Matches(issue int) bool {
	if len(p.issues) == 0 {
		return true
	}
	for _, i := range p.issues {
		if issue >= i.from && issue <= i.to {
			return true
		}
	}
	return false
}

// parseIssues ensure issue items are valid input data, i.e castable
// to int, ranges allowed.
func (p *project) parseIssues() error {
	p.issues = make([]issueRange, 0)
	var x [2]int
	for _, i := range p.Issues {
		vals := strings.Split(i, "-")
		if len(vals) > 2 {
			return fmt.Errorf("only one range separator allowed, '%s' not supported", vals)
		}
		if len(vals) > 1 {
			for k, p := range vals {
				num, err := strconv.ParseUint(p, 10, 64)
				if err != nil {
					return fmt.Errorf("wrong issue range in '%s': expects an integer, not '%s'", i, p)
				}
				x[k] = int(num)
			}
			if x[0] > x[1] {
				return fmt.Errorf("reverse range not allowed in '%s'", i)
			}
		} else {
			// No range
			num, err := strconv.ParseUint(vals[0], 10, 64)
			if err != nil {
				return fmt.Errorf("wrong issue value for '%s': expects an integer, not '%s'", i, vals[0])
			}
			x[0] = int(num)
			x[1] = int(num)
		}
		p.issues = append(p.issues, issueRange{from: x[0], to: x[1]})
	}
	return nil
}

func (p *project) checkData(prefix string) error {
	if p == nil {
		return fmt.Errorf("missing %s project's data", prefix)
	}
	if p.ServerURL == "" {
		return fmt.Errorf("missing %s project's server URL", prefix)
	}
	u, err := url.Parse(p.ServerURL)
	if err != nil {
		return err
	}
	if !strings.HasSuffix(p.ServerURL, apiPath) {
		p.ServerURL = path.Join(u.Host, u.Path, apiPath)
		p.ServerURL = fmt.Sprintf("%s://%s", u.Scheme, p.ServerURL)
	}
	if p.Name == "" {
		return fmt.Errorf("missing %s project's name", prefix)
	}
	if p.Token == "" {
		return fmt.Errorf("missing %s project's token", prefix)
	}
	return nil
}
