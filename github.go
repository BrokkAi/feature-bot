package featurebot

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BrokkAi/feature-bot/internal/osrun"
)

type Issue struct {
	Number      int             `json:"number"`
	Title       string          `json:"title"`
	Body        string          `json:"body"`
	URL         string          `json:"html_url"`
	State       string          `json:"state"`
	Comments    []string        `json:"discussion,omitempty"`
	PullRequest json.RawMessage `json:"pull_request,omitempty"`
}
type issueSource interface {
	issues(context.Context) ([]Issue, error)
	create(context.Context, *Candidate, string) (*Issue, error)
}
type githubClient struct{ config Config }

func (g githubClient) api(ctx context.Context, endpoint string, result any, fields ...string) error {
	out, err := g.request(ctx, endpoint, fields...)
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(out), result)
}
func (g githubClient) request(ctx context.Context, endpoint string, fields ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	args := []string{"gh", "api", "--hostname", g.config.GitHub.Host, endpoint}
	return osrun.Run(ctx, "", nil, append(args, fields...)...)
}
func (g githubClient) path(suffix string) string { return "repos/" + g.config.GitHubRepo() + suffix }

// Never use GitHub search's result cap, open-only queries, or label filtering for deduplication.
func (g githubClient) issues(ctx context.Context) ([]Issue, error) {
	all, err := pages[Issue](ctx, g, g.path("/issues"), url.Values{"state": {"all"}, "sort": {"created"}, "direction": {"asc"}})
	if err != nil {
		return nil, err
	}
	byNumber := map[int]int{}
	var issues []Issue
	for _, i := range all {
		if len(i.PullRequest) > 0 && string(i.PullRequest) != "null" {
			continue
		}
		if i.Number < 1 || (i.State != "open" && i.State != "closed") {
			return nil, errors.New("incomplete GitHub issue response")
		}
		if _, exists := byNumber[i.Number]; exists {
			return nil, errors.New("unstable issue pagination; retry snapshot")
		}
		byNumber[i.Number] = len(issues)
		issues = append(issues, i)
	}
	// One paginated repository-wide request includes discussion on closed issues too.
	comments, err := pages[struct {
		IssueURL string `json:"issue_url"`
		Body     string `json:"body"`
	}](ctx, g, g.path("/issues/comments"), url.Values{"sort": {"created"}, "direction": {"asc"}})
	if err != nil {
		return nil, err
	}
	for _, c := range comments {
		u, err := url.Parse(c.IssueURL)
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(u.Path[strings.LastIndex(u.Path, "/")+1:])
		if err != nil {
			return nil, errors.New("invalid comment issue URL")
		}
		if at, ok := byNumber[n]; ok {
			issues[at].Comments = append(issues[at].Comments, c.Body)
		}
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	return issues, nil
}
func pages[T any](ctx context.Context, g githubClient, path string, q url.Values) ([]T, error) {
	var all []T
	for page := 1; page <= 10000; page++ {
		q.Set("per_page", "100")
		q.Set("page", fmt.Sprint(page))
		var items []T
		if err := g.api(ctx, path+"?"+q.Encode(), &items); err != nil {
			return nil, err
		}
		if items == nil {
			return nil, errors.New("expected a GitHub array response")
		}
		all = append(all, items...)
		if len(items) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("history exceeded pagination limit; refusing incomplete duplicate check")
}
func marker(requestID string) string { return "<!-- feature-bot: " + requestID + " -->" }
func issueBody(c *Candidate, commit string) string {
	f := c.Finding
	return fmt.Sprintf("## User problem\n\n%s\n\n## Current workflow\n\n%s\n\n## Proposed feature\n\n%s\n\n## User value\n\n%s\n\n## Scope and non-goals\n\n%s\n\n## Acceptance criteria\n\n- [ ] %s\n\n## Relevant source\n\n%s\n\n## Evidence\n\n%s\n\n## Independent review\n\n%s\n\nResearched at commit `%s` by feature-bot.\n\n%s\n", f.UserProblem, f.CurrentWorkflow, f.ProposedSolution, f.UserValue, f.Scope, strings.Join(f.AcceptanceCriteria, "\n- [ ] "), strings.Join(f.Files, "\n"), strings.Join(f.Evidence, "\n\n"), c.Review, commit, marker(c.RequestID))
}
func (g githubClient) create(ctx context.Context, c *Candidate, commit string) (*Issue, error) {
	fields := []string{"--method", "POST", "--include", "-f", "title=" + c.Finding.Title, "-f", "body=" + issueBody(c, commit)}
	for _, l := range g.config.Labels {
		fields = append(fields, "-f", "labels[]="+l)
	}
	out, runErr := g.request(ctx, g.path("/issues"), fields...)
	i, err := createdResponse(out, runErr)
	if err != nil {
		return nil, err
	}
	if err := validateCreated(g.config, c, i); err != nil {
		return nil, err
	}
	return i, nil
}

// Only a confirmed rejection can make a create safe to retry. Missing responses,
// timeouts, server errors and malformed success bodies remain ambiguous.
type rejectedCreateError struct{ error }

func (e *rejectedCreateError) Unwrap() error { return e.error }

func createdResponse(out string, runErr error) (*Issue, error) {
	reader := textproto.NewReader(bufio.NewReader(strings.NewReader(out)))
	line, err := reader.ReadLine()
	if err != nil {
		return nil, errors.Join(runErr, fmt.Errorf("missing create HTTP status: %w", err))
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return nil, errors.Join(runErr, errors.New("invalid create HTTP status"))
	}
	_, _, protocolOK := http.ParseHTTPVersion(fields[0])
	status, err := strconv.Atoi(fields[1])
	if !protocolOK || err != nil || len(fields[1]) != 3 || status < 100 || status > 599 {
		return nil, errors.Join(runErr, errors.New("invalid create HTTP status"))
	}
	if _, err := reader.ReadMIMEHeader(); err != nil {
		return nil, errors.Join(runErr, fmt.Errorf("invalid create HTTP headers: %w", err))
	}
	switch status {
	case 400, 401, 403, 404, 405, 409, 410, 411, 413, 414, 415, 422, 429:
		return nil, &rejectedCreateError{errors.Join(fmt.Errorf("GitHub rejected issue creation (HTTP %d)", status), runErr)}
	}
	if runErr != nil {
		return nil, runErr
	}
	if status != http.StatusCreated {
		return nil, fmt.Errorf("unexpected create HTTP status %d", status)
	}
	// gh has already decoded HTTP transfer/content encodings. Read its printed
	// JSON directly instead of applying the response headers' encodings again.
	body, err := io.ReadAll(reader.R)
	if err != nil {
		return nil, err
	}
	var issue Issue
	if err := json.Unmarshal(body, &issue); err != nil {
		return nil, err
	}
	return &issue, nil
}

func validateCreated(cfg Config, c *Candidate, i *Issue) error {
	if i == nil || i.Number < 1 || !strings.Contains(i.Body, marker(c.RequestID)) || (len(i.PullRequest) > 0 && string(i.PullRequest) != "null") {
		return errors.New("created issue does not match repository and finding")
	}
	u, err := url.Parse(i.URL)
	suffix := fmt.Sprintf("/issues/%d", i.Number)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" ||
		!strings.EqualFold(u.Host, cfg.GitHub.Host) || !strings.HasSuffix(u.Path, suffix) || !strings.EqualFold(strings.TrimSuffix(u.Path, suffix), "/"+cfg.GitHubRepo()) {
		return errors.New("created issue does not match repository and finding")
	}
	return nil
}
