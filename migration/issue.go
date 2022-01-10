package migration

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/gotsunami/gitlab-copy/config"
	"github.com/gotsunami/gitlab-copy/gitlab"
	glab "github.com/xanzy/go-gitlab"
)

var (
	errDuplicateIssue = errors.New("Duplicate Issue")
	AttachmentRegex   = regexp.MustCompile(`\[.*?\]\(/uploads/(?P<link>.*?)\)`)
)

const (
	// ResultsPerPage is the Number of results per page.
	ResultsPerPage = 100
)

// Endpoint refers to the GitLab server endpoints.
type Endpoint struct {
	SrcClient, DstClient gitlab.GitLaber
}

// Migration defines a migration step.
type Migration struct {
	params                 *config.Config
	Endpoint               *Endpoint
	srcProject, dstProject *glab.Project
	toUsers                map[string]gitlab.GitLaber
	skipIssue              bool
}

// Creates a new migration.
func New(c *config.Config) (*Migration, error) {
	if c == nil {
		return nil, errors.New("nil params")
	}
	m := &Migration{params: c}
	m.toUsers = make(map[string]gitlab.GitLaber)

	fromgl := gitlab.CreateNew().New(nil, c.SrcPrj.Token)
	if err := fromgl.SetBaseURL(c.SrcPrj.ServerURL); err != nil {
		return nil, err
	}
	togl := gitlab.CreateNew().New(nil, c.DstPrj.Token)
	if err := togl.SetBaseURL(c.DstPrj.ServerURL); err != nil {
		return nil, err
	}
	for user, token := range c.DstPrj.Users {
		uc := gitlab.CreateNew().New(nil, token)
		if err := uc.SetBaseURL(c.DstPrj.ServerURL); err != nil {
			return nil, err
		}
		m.toUsers[user] = uc
	}
	m.Endpoint = &Endpoint{fromgl, togl}
	return m, nil
}

// Returns project by name.
func (m *Migration) project(endpoint gitlab.GitLaber, name, which string) (*glab.Project, error) {
	proj, resp, err := endpoint.GetProject(name)
	if resp == nil {
		return nil, errors.New("network error while fetching project info: nil response")
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("%s project '%s' not found", which, name)
	}
	if err != nil {
		return nil, err
	}
	return proj, nil
}

func (m *Migration) SourceProject(name string) (*glab.Project, error) {
	p, err := m.project(m.Endpoint.SrcClient, name, "source")
	if err != nil {
		return nil, err
	}
	m.srcProject = p
	return p, nil
}

func (m *Migration) DestProject(name string) (*glab.Project, error) {
	p, err := m.project(m.Endpoint.DstClient, name, "target")
	if err != nil {
		return nil, err
	}
	m.dstProject = p
	return p, nil
}

func (m *Migration) migrateIssue(issueID int) error {
	source := m.Endpoint.SrcClient
	target := m.Endpoint.DstClient

	srcProjectID := m.srcProject.ID
	tarProjectID := m.dstProject.ID

	issue, _, err := source.GetIssue(srcProjectID, issueID)
	if err != nil {
		return fmt.Errorf("target: can't fetch issue: %s", err.Error())
	}

	tis := make([]*glab.Issue, 0)
	curPage := 1
	optSort := "asc"
	isopts := &glab.ListProjectIssuesOptions{Sort: &optSort, ListOptions: glab.ListOptions{PerPage: ResultsPerPage, Page: curPage}}
	for {
		issues, _, err := target.ListProjectIssues(srcProjectID, isopts)
		if err != nil {
			return err
		}
		if len(issues) == 0 {
			break
		}

		for _, issue := range issues {
			tis = append(tis, issue)
		}
		curPage++
		isopts.Page = curPage
	}
	if err != nil {
		return fmt.Errorf("target: can't fetch issue: %s", err.Error())
	}
	for _, t := range tis {
		if issue.IID == t.IID || issue.Title == t.Title {
			if issue.State == "closed" {
				event := "close"
				_, _, err := target.UpdateIssue(tarProjectID, t.IID, &glab.UpdateIssueOptions{StateEvent: &event, Labels: issue.Labels})
				if err != nil {
					return fmt.Errorf("target: error closing issue #%d: %s", t.IID, err.Error())
				}
			}

			// Target issue already exists, let's skip this one.
			return errDuplicateIssue
		}
	}
	iopts := &glab.CreateIssueOptions{
		Title:       &issue.Title,
		Description: &issue.Description,
		Labels:      make([]string, 0),
	}
	if issue.Assignee.Username != "" {
		// Assigned, does target user exist?
		// User may have a different ID on target
		u, err := m.GetUserID(target, issue.Assignee.Username)
		if err == nil {
			if u > -1 {
				iopts.AssigneeIDs = []int{u}
			}
		} else {
			return fmt.Errorf("target: error fetching users: %s", err.Error())
		}
	}
	if issue.Milestone != nil && issue.Milestone.Title != "" {
		miles, _, err := target.ListMilestones(tarProjectID, nil)
		if err == nil {
			found := false
			for _, mi := range miles {
				found = false
				if mi.Title == issue.Milestone.Title {
					found = true
					iopts.MilestoneID = &mi.ID
					break
				}
			}
			if !found {
				// Create target milestone
				cmopts := &glab.CreateMilestoneOptions{
					Title:       &issue.Milestone.Title,
					Description: &issue.Milestone.Description,
					DueDate:     issue.Milestone.DueDate,
				}
				mi, _, err := target.CreateMilestone(tarProjectID, cmopts)
				if err == nil {
					iopts.MilestoneID = &mi.ID
				} else {
					return fmt.Errorf("target: error creating milestone '%s': %s", issue.Milestone.Title, err.Error())
				}
			}
		} else {
			return fmt.Errorf("target: error listing milestones: %s", err.Error())
		}
	}
	// Copy existing labels.
	for _, label := range issue.Labels {
		iopts.Labels = append(iopts.Labels, label)
	}

	// Create target issue if not existing (same name).
	if _, ok := m.toUsers[issue.Author.Username]; ok {
		target = m.toUsers[issue.Author.Username]
	}

	desc, err := m.DownloadAttachments(source, target, *iopts.Description)
	if err != nil {
		return fmt.Errorf("target: error downloading issue attachments (%s): %s", issue.Author.Username, err.Error())
	} else {
		iopts.Description = &desc
	}

	bottom := fmt.Sprintf("*By %s (@%s) on %s (imported from GitLab)*", issue.Author.Name, issue.Author.Username, issue.CreatedAt.Format(time.RFC1123))
	origLink := fmt.Sprintf("*[Original issue #%d](%s)*", issue.IID, issue.WebURL)
	desc = fmt.Sprintf("%s\n\n%s\n\n%s", *iopts.Description, bottom, origLink)
	iopts.Description = &desc

	ni, resp, err := target.CreateIssue(tarProjectID, iopts)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusRequestURITooLong {
			fmt.Printf("target: caught a %q error, shortening issue's decription length ...\n", http.StatusText(resp.StatusCode))
			if len(*iopts.Description) == 0 {
				return fmt.Errorf("target: error creating issue: no description but %q error ...\n", http.StatusText(resp.StatusCode))
			}
			smalld := (*iopts.Description)[:1024]
			iopts.Description = &smalld
			ni, _, err = target.CreateIssue(tarProjectID, iopts)
			if err != nil {
				return fmt.Errorf("target: error creating empty issue: %s user: %s", err.Error(), issue.Author.Username)
			}
		} else {
			return fmt.Errorf("target: error creating issue: %s user: %s", err.Error(), issue.Author.Username)
		}
	}

	target = m.Endpoint.DstClient

	// Copy related notes (comments)
	notes, _, err := source.ListIssueNotes(srcProjectID, issue.IID, nil)
	if err != nil {
		return fmt.Errorf("source: can't get issue #%d notes: %s", issue.IID, err.Error())
	}
	opts := &glab.CreateIssueNoteOptions{}
	// Notes on target will be added in reverse order.
	for j := len(notes) - 1; j >= 0; j-- {
		n := notes[j]
		target = m.Endpoint.DstClient
		// Can we write the comment with user ownership?
		if _, ok := m.toUsers[n.Author.Username]; ok {
			target = m.toUsers[n.Author.Username]
		}
		opts.Body = &n.Body
		if strings.Contains(*opts.Body, "created merge request ") {
			continue
		}
		if strings.Contains(*opts.Body, "mentioned in ") {
			continue
		}
		if strings.Contains(*opts.Body, "changed time estimate to ") {
			continue
		}
		if strings.Contains(*opts.Body, "changed due date to ") {
			continue
		}
		if strings.Contains(*opts.Body, "assigned to") {
			continue
		}
		if strings.Contains(*opts.Body, "changed the description") {
			continue
		}
		if strings.Contains(*opts.Body, "changed title from") {
			continue
		}
		if strings.Contains(*opts.Body, "removed due date") {
			continue
		}
		if strings.Contains(*opts.Body, "marked this issue as related to ") {
			continue
		}
		if strings.Contains(*opts.Body, "made the issue visible to everyone") {
			continue
		}
		if strings.Contains(*opts.Body, "added ") && strings.HasSuffix(*opts.Body, " of time spent") {
			continue
		}

		desc, err := m.DownloadAttachments(source, target, *opts.Body)
		if err != nil {
			return fmt.Errorf("target: error downloading attachments (%s): %s", n.Author.Username, err.Error())
		} else {
			opts.Body = &desc
		}

		bottom := fmt.Sprintf("*By %s (@%s) on %s (imported from GitLab)*", n.Author.Name, n.Author.Username, n.CreatedAt.Format(time.RFC1123))
		desc = fmt.Sprintf("%s\n\n%s", *opts.Body, bottom)
		opts.Body = &desc

		_, resp, err := target.CreateIssueNote(tarProjectID, ni.IID, opts)
		if err != nil {
			if resp.StatusCode == http.StatusRequestURITooLong {
				fmt.Printf("target: note's body too long, shortening it ...\n")
				if len(*opts.Body) > 1024 {
					smallb := (*opts.Body)[:1024]
					opts.Body = &smallb
				}
				_, _, err := target.CreateIssueNote(tarProjectID, ni.IID, opts)
				if err != nil {
					return fmt.Errorf("target: error creating note (with shorter body) for issue #%d: %s", ni.IID, err.Error())
				}
			} else {
				return fmt.Errorf("target: error creating note for issue #%d: %s", ni.IID, err.Error())
			}
		}
	}
	target = m.Endpoint.DstClient

	if issue.State == "closed" {
		event := "close"
		_, _, err := target.UpdateIssue(tarProjectID, ni.IID, &glab.UpdateIssueOptions{StateEvent: &event, Labels: issue.Labels})
		if err != nil {
			return fmt.Errorf("target: error closing issue #%d: %s", ni.IID, err.Error())
		}
	}
	// Add a link to target issue if needed
	if m.params.SrcPrj.LinkToTargetIssue {
		var dstProjectURL string
		// Strip URL if moving on the same GitLab  installation.
		if m.Endpoint.SrcClient.BaseURL().Host == m.Endpoint.DstClient.BaseURL().Host {
			dstProjectURL = m.dstProject.PathWithNamespace
		} else {
			dstProjectURL = m.dstProject.WebURL
		}
		tmpl, err := template.New("link").Parse(m.params.SrcPrj.LinkToTargetIssueText)
		if err != nil {
			return fmt.Errorf("link to target issue: error parsing linkToTargetIssueText parameter: %s", err.Error())
		}
		noteLink := fmt.Sprintf("%s#%d", dstProjectURL, ni.IID)
		type link struct {
			Link string
		}
		buf := new(bytes.Buffer)
		if err := tmpl.Execute(buf, &link{noteLink}); err != nil {
			return fmt.Errorf("link to target issue: %s", err.Error())
		}
		nopt := buf.String()
		opts := &glab.CreateIssueNoteOptions{Body: &nopt}
		_, _, err = target.CreateIssueNote(srcProjectID, issue.IID, opts)
		if err != nil {
			return fmt.Errorf("source: error adding closing note for issue #%d: %s", issue.IID, err.Error())
		}
	}
	// Auto close source issue if needed
	if m.params.SrcPrj.AutoCloseIssues {
		event := "close"
		_, _, err := source.UpdateIssue(srcProjectID, issue.ID, &glab.UpdateIssueOptions{StateEvent: &event, Labels: issue.Labels})
		if err != nil {
			return fmt.Errorf("source: error closing issue #%d: %s", issue.IID, err.Error())
		}
	}

	fmt.Printf("target: created issue #%d: %s [%s]\n", ni.IID, ni.Title, issue.State)
	return nil
}

func (m *Migration) GetUserID(target gitlab.GitLaber, username string) (int, error) {
	users, _, err := target.ListUsers(nil)
	if err == nil {
		if _, ok := m.toUsers[username]; ok {
			usr := m.toUsers[username]
			u, _, err := usr.CurrentUser()
			if err != nil {
				return -1, fmt.Errorf("failed using the API with user '%s': %s", username, err.Error())
			}
			return u.ID, nil
		} else {
			for _, u := range users {
				if u.Username == username {
					return u.ID, nil
				}
			}
		}
		return -1, nil
	} else {
		return -1, fmt.Errorf("target: error fetching users: %s", err.Error())
	}
}

func (m *Migration) DownloadAttachments(source gitlab.GitLaber, target gitlab.GitLaber, text string) (string, error) {
	if source.BaseURL().Host == target.BaseURL().Host {
		return text, nil //same server, not need to transfer files
	}
	matches := AttachmentRegex.FindAllStringSubmatch(text, -1)
	for _, v := range matches {
		for kk, vv := range AttachmentRegex.SubexpNames() {
			if vv == "link" {
				link := v[kk]

				dir, err := ioutil.TempDir(os.TempDir(), "tmp")
				file, err := os.Create(path.Join(dir, filepath.Base(link)))
				if err != nil {
					return text, err
				}
				defer file.Close()

				// Get the data
				u, err := url.Parse(m.srcProject.WebURL)
				if err != nil {
					return text, err
				}

				u.Path = path.Join(u.Path, "/uploads/", link)
				resp, err := http.Get(u.String())
				if err != nil {
					return text, err
				}
				defer resp.Body.Close()

				// Check server response
				if resp.StatusCode != http.StatusOK {
					return text, fmt.Errorf("bad status: %s", resp.Status)
				}

				// Writer the body to file
				_, err = io.Copy(file, resp.Body)
				if err != nil {
					return text, err
				}

				response, _, err := target.UploadFile(m.dstProject.ID, file.Name())
				defer os.RemoveAll(dir)
				if err != nil {
					return text, err
				}

				text = strings.Replace(text, "/uploads/"+link, response.URL, -1)
			}
		}
	}
	return text, nil
}

type issueId struct {
	IID, ID int
}

type byIID []issueId

func (a byIID) Len() int           { return len(a) }
func (a byIID) Swap(i, j int)      { a[i], a[j] = a[j], a[i] }
func (a byIID) Less(i, j int) bool { return a[i].IID < a[j].IID }

// Performs the issues migration.
func (m *Migration) Migrate() error {
	_, err := m.SourceProject(m.params.SrcPrj.Name)
	if err != nil {
		return err
	}
	_, err = m.DestProject(m.params.DstPrj.Name)
	if err != nil {
		return err
	}

	source := m.Endpoint.SrcClient
	target := m.Endpoint.DstClient

	srcProjectID := m.srcProject.ID
	tarProjectID := m.dstProject.ID

	curPage := 1
	optSort := "asc"
	opts := &glab.ListProjectIssuesOptions{Sort: &optSort, ListOptions: glab.ListOptions{PerPage: ResultsPerPage, Page: curPage}}

	s := make([]issueId, 0)

	// Copy all source labels on target
	labels, _, err := source.ListLabels(srcProjectID, nil)
	if err != nil {
		return fmt.Errorf("source: can't fetch labels: %s", err.Error())
	}
	fmt.Printf("Found %d labels ...\n", len(labels))
	for _, label := range labels {
		clopts := &glab.CreateLabelOptions{Name: &label.Name, Color: &label.Color, Description: &label.Description}
		_, resp, err := target.CreateLabel(tarProjectID, clopts)
		if err != nil {
			// GitLab returns a 409 code if label already exists
			if resp.StatusCode != http.StatusConflict {
				return fmt.Errorf("target: error creating label '%s': %s", label, err.Error())
			}
		}
	}

	if m.params.SrcPrj.LabelsOnly {
		// We're done here
		return nil
	}

	if m.params.SrcPrj.MilestonesOnly {
		fmt.Println("Copying milestones ...")
		miles, _, err := source.ListMilestones(srcProjectID, nil)
		if err != nil {
			return fmt.Errorf("error getting the milestones from source project: %s", err.Error())
		}
		fmt.Printf("Found %d milestones\n", len(miles))
		for _, mi := range miles {
			// Create target milestone
			cmopts := &glab.CreateMilestoneOptions{
				Title:       &mi.Title,
				Description: &mi.Description,
				DueDate:     mi.DueDate,
			}
			tmi, _, err := target.CreateMilestone(tarProjectID, cmopts)
			if err != nil {
				return fmt.Errorf("target: error creating milestone '%s': %s\n", mi.Title, err.Error())
			}
			if mi.State == "closed" {
				event := "close"
				umopts := &glab.UpdateMilestoneOptions{
					StateEvent: &event,
				}
				_, _, err := target.UpdateMilestone(tarProjectID, tmi.ID, umopts)
				if err != nil {
					return fmt.Errorf("target: error closing milestone '%s': %s\n", mi.Title, err.Error())
				}
			}
		}
		// We're done here
		return nil
	}

	fmt.Println("Copying issues ...")

	// First, count issues
	for {
		issues, _, err := source.ListProjectIssues(srcProjectID, opts)
		if err != nil {
			return err
		}
		if len(issues) == 0 {
			break
		}

		for _, issue := range issues {
			s = append(s, issueId{IID: issue.IID, ID: issue.ID})
		}
		curPage++
		opts.Page = curPage
	}

	// Then sort
	sort.Sort(byIID(s))

	for _, issue := range s {
		if m.params.SrcPrj.Matches(issue.IID) {
			if err := m.migrateIssue(issue.IID); err != nil {
				if err == errDuplicateIssue {
					fmt.Printf("target: issue %d already exists, skipping...\n", issue.IID)
					continue
				}
				return err
			}
			if m.params.SrcPrj.MoveIssues {
				// Delete issue from source project
				_, err := source.DeleteIssue(srcProjectID, issue.ID)
				if err != nil {
					log.Printf("could not delete the issue %d: %s\n", issue.ID, err.Error())
				}
			}
		}
	}

	return nil
}
