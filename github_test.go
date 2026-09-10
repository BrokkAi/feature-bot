package featurebot

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A real gh subprocess fixture exercises endpoint parameters, complete pagination,
// PR exclusion, closed-issue discussion, structured fields and response checks.
func githubFixture(t *testing.T) githubClient {
	t.Helper()
	dir := canonicalTestDir(t)
	script := `#!/usr/bin/env python3
import json, os, sys
from urllib.parse import urlsplit, parse_qs
args = sys.argv[1:]
assert args[:3] == ['api', '--hostname', 'github.com']
url = urlsplit(args[3]); q = parse_qs(url.query)
mode = os.environ.get('FEATURE_BOT_TEST_MODE', '')
if '--method' in args:
    assert '--include' in args
    assert args[args.index('--method')+1] == 'POST'
    assert url.path == 'repos/o/r/issues'
    fields = [args[n+1] for n,x in enumerate(args) if x == '-f']
    body = next(x[5:] for x in fields if x.startswith('body='))
    title = next(x[6:] for x in fields if x.startswith('title='))
    assert 'labels[]=enhancement' in fields and 'labels[]=triage' in fields
    print('HTTP/2.0 201 Created\r\nContent-Type: application/json\r\n\r\n', end='')
    print(json.dumps(dict(number=42, title=title, body=body, state='open', html_url='https://github.com/o/r/issues/42' if mode != 'wrong_repo' else 'https://github.com/other/repo/issues/42')))
    sys.exit()
page = int(q['page'][0]); assert q['per_page'] == ['100']
assert q['sort'] == ['created'] and q['direction'] == ['asc']
if url.path.endswith('/issues/comments'):
    if mode == 'comments_error': sys.exit(1)
    numbers = range(1,101) if page == 1 else [101]
    print(json.dumps([dict(issue_url=f'https://api.github.com/repos/o/r/issues/{n}', body=f'discussion {n}') for n in numbers]))
else:
    assert url.path == 'repos/o/r/issues'
    assert q['state'] == ['all'] and 'labels' not in q
    if page == 2 and mode == 'page_error': sys.exit(1)
    if page == 2 and mode == 'null_page': print('null'); sys.exit()
    numbers = range(1,101) if page == 1 else [100 if mode == 'repeated_page' else 101]
    items = [dict(number=n, title=f'Issue {n}', body='body', state='closed' if n == 101 else 'open', html_url=f'https://github.com/o/r/issues/{n}') for n in numbers]
    if page == 1: items[0]['pull_request'] = dict(url='https://api.github.com/repos/o/r/pulls/1')
    print(json.dumps(items))
`
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	cfg := DefaultConfig()
	cfg.Remote = "https://github.com/o/r.git"
	cfg.Labels = []string{"enhancement", "triage"}
	return githubClient{cfg}
}
func TestGitHubAllIssuesAndCommentsPaginate(t *testing.T) {
	g := githubFixture(t)
	issues, err := g.issues(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 100 || issues[0].Number != 2 {
		t.Fatalf("missing issues or included PR: %d", len(issues))
	}
	last := issues[len(issues)-1]
	if last.Number != 101 || last.State != "closed" || len(last.Comments) != 1 || last.Comments[0] != "discussion 101" {
		t.Fatalf("closed issue discussion missing: %+v", last)
	}
}
func TestGitHubIncompleteHistoryRejected(t *testing.T) {
	for _, mode := range []string{"page_error", "null_page", "repeated_page", "comments_error"} {
		t.Run(mode, func(t *testing.T) {
			g := githubFixture(t)
			t.Setenv("FEATURE_BOT_TEST_MODE", mode)
			if _, err := g.issues(context.Background()); err == nil {
				t.Fatal("accepted incomplete history")
			}
		})
	}
}
func TestGitHubCreateFieldsAndIdentity(t *testing.T) {
	g := githubFixture(t)
	c := &Candidate{RequestID: "0123456789abcdef0123456789abcdef", Finding: finding(), Review: "Verified"}
	i, err := g.create(context.Background(), c, "0123456789abcdef0123456789abcdef01234567")
	if err != nil {
		t.Fatal(err)
	}
	if i.Number != 42 || !containsMarker(*i, c.RequestID) {
		t.Fatal("missing receipt marker")
	}
	for _, text := range []string{c.Finding.UserProblem, c.Finding.CurrentWorkflow, c.Finding.ProposedSolution, c.Finding.UserValue, c.Finding.Scope, "## Acceptance criteria", "- [ ] " + c.Finding.AcceptanceCriteria[0], "<!-- feature-bot: " + c.RequestID + " -->"} {
		if !strings.Contains(i.Body, text) {
			t.Errorf("created issue omitted %q", text)
		}
	}
	if containsMarker(Issue{Body: "<!-- bug-bot: " + c.RequestID + " -->"}, c.RequestID) {
		t.Fatal("bug-bot marker was mistaken for a feature publication")
	}
	t.Setenv("FEATURE_BOT_TEST_MODE", "wrong_repo")
	if _, err := g.create(context.Background(), c, "0123456789abcdef0123456789abcdef01234567"); err == nil {
		t.Fatal("wrong repository response accepted")
	}
}

func TestCreateResponseDistinguishesRejectionFromUnknownOutcome(t *testing.T) {
	for _, tc := range []struct {
		name, output string
		runErr       error
		rejected     bool
	}{
		{"validation", "HTTP/2.0 422 Unprocessable Entity\nContent-Type: application/json\r\n\r\n{}", errors.New("exit 1"), true},
		{"forbidden", "HTTP/1.1 403 Forbidden\r\n\r\n{}", errors.New("exit 1"), true},
		{"rate limit", "HTTP/2.0 429 Too Many Requests\r\n\r\n{}", errors.New("exit 1"), true},
		{"server error", "HTTP/2.0 500 Internal Server Error\r\n\r\n{}", errors.New("exit 1"), false},
		{"request timeout", "HTTP/1.1 408 Request Timeout\r\n\r\n{}", errors.New("exit 1"), false},
		{"proxy client close", "HTTP/1.1 499 Client Closed Request\r\n\r\n{}", errors.New("exit 1"), false},
		{"lost response", "", errors.New("connection lost"), false},
		{"stderr is not status", "", errors.New("gh: HTTP 422"), false},
		{"body is not status", `{"message":"HTTP/2.0 422 Unprocessable Entity"}`, errors.New("exit 1"), false},
		{"incomplete headers", "HTTP/2.0 422 Unprocessable Entity\r\nContent-Type:", errors.New("connection lost"), false},
		{"invalid protocol", "HTTP/garbage 422 Rejected\r\n\r\n{}", errors.New("exit 1"), false},
		{"invalid status", "HTTP/2.0 0422 Rejected\r\n\r\n{}", errors.New("exit 1"), false},
		{"malformed success", "HTTP/2.0 201 Created\r\n\r\n{", nil, false},
		{"failed successful response", "HTTP/2.0 201 Created\r\n\r\n{}", errors.New("connection lost"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := createdResponse(tc.output, tc.runErr)
			var rejected *rejectedCreateError
			if err == nil || errors.As(err, &rejected) != tc.rejected {
				t.Fatalf("got %v, want rejected=%t", err, tc.rejected)
			}
		})
	}
	// gh prints a decoded body even when the original response was chunked.
	i, err := createdResponse("HTTP/1.1 201 Created\r\nTransfer-Encoding: chunked\r\n\r\n{\"number\":42}", nil)
	if err != nil || i.Number != 42 {
		t.Fatalf("decoded response: %+v %v", i, err)
	}
}

func TestCreatedIdentityAllowsCanonicalCaseOnly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.GitHub.Repo = "Owner/Repo"
	c := &Candidate{RequestID: strings.Repeat("a", 32)}
	for _, tc := range []struct {
		url string
		ok  bool
	}{
		{"https://github.com/owner/repo/issues/42", true},
		{"https://GITHUB.COM/OWNER/REPO/issues/42", true},
		{"https://github.com/other/repo/issues/42", false},
		{"https://other.example/owner/repo/issues/42", false},
		{"http://github.com/owner/repo/issues/42", false},
		{"https://user@github.com/owner/repo/issues/42", false},
		{"https://github.com/owner/repo/pulls/42", false},
		{"https://github.com/owner/repo/issues/43", false},
		{"https://github.com/owner/repo/issues/042", false},
		{"https://github.com/owner/repo/issues/42?query=1", false},
		{"https://github.com/owner/repo/issues/42?", false},
		{"https://github.com/owner/repo/issues/42#comment", false},
		{"https://github.com/%6fwner/repo/issues/42", false},
	} {
		i := &Issue{Number: 42, URL: tc.url, Body: marker(c.RequestID)}
		if err := validateCreated(cfg, c, i); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.url, err)
		}
	}
}
