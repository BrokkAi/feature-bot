package featurebot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reproduces the real ACP message sequence that previously lost valid receipts.
func TestFinalReceiptAfterCommentary(t *testing.T) {
	for _, verdict := range []string{"new", "duplicate", "uncertain", "invalid"} {
		t.Run(verdict, func(t *testing.T) { testFinalReceiptAfterCommentary(t, verdict) })
	}
}

func testFinalReceiptAfterCommentary(t *testing.T, verdict string) {
	e, s, source, _, _ := fixture(t)
	review := Review{Verdict: verdict, Reason: "Independently checked the capability gap, repository fit and proposal history", Checked: []int{}}
	if verdict == "duplicate" {
		source.items = []Issue{{Number: 77, State: "closed", Title: "Download task rows", Comments: []string{"Rejected spreadsheet export proposal"}}}
		review.Checked, review.Duplicate = []int{77}, 77
	}
	script := filepath.Join(canonicalTestDir(t), "agent.py")
	writeTestFile(t, script, `import json, os, sys
def send(value):
    print(json.dumps(value), flush=True)
def chunk(message, phase, text, kind='agent_message_chunk'):
    send(dict(jsonrpc='2.0', method='session/update', params=dict(
        sessionId='fixture', update=dict(sessionUpdate=kind, messageId=message,
        content=dict(type='text', text=text), _meta=dict(codex=dict(phase=phase))))))
for line in sys.stdin:
    request = json.loads(line)
    method = request.get('method')
    if method == 'initialize':
        result = dict(protocolVersion=1, agentCapabilities={}, authMethods=[])
    elif method == 'session/new':
        result = dict(sessionId='fixture')
    elif method == 'session/prompt':
        prompt = request['params']['prompt'][0]['text']
        assert 'studious feature researcher' in prompt
        if 'Scan context (data):' in prompt:
            assert 'Find NEW features' in prompt
            for field in ['user_problem', 'current_workflow', 'proposed_solution', 'user_value', 'scope', 'acceptance_criteria', 'evidence']:
                assert field in prompt
            chunk('progress', 'commentary', 'The investigation is complete.')
            chunk('scan', 'final_answer', os.environ['FEATURE_DIG_SCAN'])
        else:
            assert 'FEATURE_REVIEW' in prompt and 'previously rejected' in prompt
            chunk('progress', 'commentary', 'The capability gap and scope were independently checked.')
            chunk('thought', 'analysis', 'Finalizing the JSON object', 'agent_thought_chunk')
            chunk('review', 'final_answer', 'FEATURE_')
            chunk('review', 'final_answer', 'REVIEW ' + os.environ['FEATURE_DIG_REVIEW'])
        result = dict(stopReason='end_turn')
    else:
        continue
    send(dict(jsonrpc='2.0', id=request['id'], result=result))
`)
	e.agent = func(cfg Config) Agent {
		cfg.Agent.Command = []string{"python3", script}
		cfg.Agent.Environment = map[string]string{
			"FEATURE_DIG_SCAN":   "FEATURE_RESULT " + jsonContextCompact(ScanResult{Summary: "Researched task export workflow", Findings: []Finding{finding()}}),
			"FEATURE_DIG_REVIEW": jsonContextCompact(review),
		}
		return agentProcess{config: cfg, log: e.log}
	}
	err := e.step(context.Background(), s, true)
	wantCreates, wantStatus := 0, verdict
	if verdict == "new" {
		wantCreates, wantStatus = 1, "submitted"
	}
	if err != nil || source.creates != wantCreates || len(s.Completed) != 1 || s.Completed[0].Status != wantStatus {
		t.Fatalf("final ACP receipt did not preserve %s outcome: err=%v, creates=%d, state=%+v", verdict, err, source.creates, s)
	}
}

func TestCanonicalRepositoryCaseReconciles(t *testing.T) {
	e, s, source, _, _ := fixture(t)
	// The fake GitHub source returns canonical o/r URLs. This accepted config
	// names the same repository with different capitalization.
	e.config.GitHub.Repo = "O/R"
	e.config.GitHub.Host = "GitHub.COM"
	s.Repo = e.config.GitHubRepo()
	s.Host = e.config.GitHub.Host
	if err := e.config.Validate(); err != nil {
		t.Fatal(err)
	}
	source.lost = true
	first := e.step(context.Background(), s, true)
	if first == nil || !strings.Contains(first.Error(), "connection lost") {
		t.Fatalf("expected lost response, got %v", first)
	}
	if source.creates != 1 || len(source.items) != 1 {
		t.Fatal("fixture did not create the issue")
	}
	saved, err := ReadState(e.config)
	if err != nil {
		t.Fatal(err)
	}
	second := e.step(context.Background(), saved, true)
	if second != nil || saved.Scan != nil || len(saved.Completed) != 1 || saved.Completed[0].Status != "submitted" || source.creates != 1 {
		t.Fatalf("same-repository issue must reconcile: first=%v; restart=%v; creates=%d", first, second, source.creates)
	}
}

func TestDefinitePostRejectionCanRetry(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(map[bool]string{false: "automatic", true: "explicit"}[explicit], func(t *testing.T) {
			testDefinitePostRejectionCanRetry(t, explicit)
		})
	}
}

func testDefinitePostRejectionCanRetry(t *testing.T, explicit bool) {
	e, s, _, _, _ := fixture(t)
	if explicit {
		e.config.Attempts = 1
	}
	dir := canonicalTestDir(t)
	gh := filepath.Join(dir, "gh")
	// A definite rejected create, as opposed to an ambiguous lost response.
	writeTestFile(t, gh, `#!/usr/bin/env python3
import json, os, sys
if '--method' in sys.argv:
    assert '--include' in sys.argv
    if os.environ.get('FEATURE_DIG_ACCEPT_POST') != '1':
        print('HTTP/2.0 422 Unprocessable Entity\r\nContent-Type: application/json\r\n\r\n{"message":"Validation Failed"}')
        print('gh: Validation Failed (HTTP 422)', file=sys.stderr)
        sys.exit(1)
    body = next(arg[5:] for arg in sys.argv if arg.startswith('body='))
    print('HTTP/2.0 201 Created\r\nContent-Type: application/json\r\n\r\n', end='')
    print(json.dumps(dict(number=1, html_url='https://github.com/o/r/issues/1', body=body)))
    sys.exit(0)
print('[]')
`)
	if err := os.Chmod(gh, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("XDG_STATE_HOME", canonicalTestDir(t))
	e.source = githubClient{config: e.config}
	first := e.step(context.Background(), s, true)
	if first == nil || !strings.Contains(first.Error(), "HTTP 422") {
		t.Fatalf("expected definite API rejection, got %v", first)
	}
	// A confirmed rejection must survive restart as pending, so correcting the
	// request allows either automatic recovery or retry after budget exhaustion.
	t.Setenv("FEATURE_DIG_ACCEPT_POST", "1")
	saved, err := ReadState(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Scan.Candidates[0].Status != "pending" {
		t.Fatalf("rejected create left status %s", saved.Scan.Candidates[0].Status)
	}
	if explicit {
		if err := Retry(e.config); err != nil {
			t.Fatal(err)
		}
		saved, err = ReadState(e.config)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := e.step(context.Background(), saved, true); err != nil {
		t.Fatal(err)
	}
	if saved.Scan != nil || len(saved.Completed) != 1 || saved.Completed[0].Status != "submitted" {
		t.Fatal("corrected create did not complete")
	}
}
