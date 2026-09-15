package featurebot

import (
	"fmt"
	"io"
	"strings"
)

// ValidateReportStatus checks a saved-status filter; empty selects all candidates.
func ValidateReportStatus(status string) error {
	switch status {
	case "", "pending", "posting", "submitted", "duplicate", "uncertain", "invalid", "dry_run", "stale":
		return nil
	default:
		return fmt.Errorf("unsupported report status %q; use pending, posting, submitted, duplicate, uncertain, invalid, dry_run, or stale (omit --status for all)", status)
	}
}

// WriteReport renders all candidates from ReadState, without exposing execution
// metadata or attributing completed findings to the active scan's commit.
// Proposal prose is preserved as Markdown and should be reviewed before sharing.
func WriteReport(w io.Writer, cfg Config, state *State, status string) error {
	if err := ValidateReportStatus(status); err != nil {
		return err
	}
	var out strings.Builder
	fmt.Fprintf(&out, "# Saved proposals\n\nRepository: %s/%s\n\nBranch: %s\n", cfg.GitHub.Host, cfg.GitHubRepo(), cfg.Branch)
	if status != "" {
		fmt.Fprintf(&out, "\nStatus filter: %s\n", status)
	}
	count := 0
	render := func(candidates []*Candidate) {
		for _, c := range candidates {
			if status != "" && c.Status != status {
				continue
			}
			count++
			f := c.Finding
			fmt.Fprintf(&out, "\n## %s\n\nStatus: %s\n", f.Title, c.Status)
			if c.URL != "" {
				fmt.Fprintf(&out, "\nIssue or duplicate URL: %s\n", c.URL)
			}
			for _, section := range [][2]string{
				{"User problem", f.UserProblem}, {"Current workflow", f.CurrentWorkflow},
				{"Proposed feature", f.ProposedSolution}, {"User value", f.UserValue},
				{"Scope and non-goals", f.Scope}, {"Acceptance criteria", reportList(f.AcceptanceCriteria)},
				{"Relevant source", reportList(f.Files)}, {"Evidence", strings.Join(f.Evidence, "\n\n")},
				{"Independent review", c.Review},
			} {
				fmt.Fprintf(&out, "\n### %s\n\n%s\n", section[0], section[1])
			}
		}
	}
	if state != nil {
		render(state.Completed)
		if state.Scan != nil {
			render(state.Scan.Candidates)
		}
	}
	if count == 0 {
		out.WriteString("\nNo saved findings match this report.\n")
	}
	_, err := io.WriteString(w, out.String())
	return err
}

func reportList(items []string) string {
	var lines []string
	for _, item := range items {
		lines = append(lines, "- "+strings.ReplaceAll(item, "\n", "\n  "))
	}
	return strings.Join(lines, "\n")
}
