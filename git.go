package featurebot

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/BrokkAi/feature-bot/internal/osrun"
)

type checkout struct{ config Config }

func (g checkout) git(ctx context.Context, args ...string) (string, error) {
	return osrun.Run(ctx, g.config.Directory, map[string]string{"GIT_TERMINAL_PROMPT": "0"}, append([]string{"git"}, args...)...)
}

func (g checkout) head(ctx context.Context) (string, error) {
	return g.git(ctx, "rev-parse", "refs/remotes/origin/"+g.config.Branch)
}
func (g checkout) prepare(ctx context.Context, s *Scan) (checkout, error) {
	w := g
	w.config.Directory = s.Directory
	if _, err := os.Stat(s.Directory); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(s.Directory), 0700); err != nil {
			return w, err
		}
		if _, err := g.git(ctx, "worktree", "add", "--detach", "--", s.Directory, s.Commit); err != nil {
			return w, err
		}
	} else if err != nil {
		return w, err
	}
	common, err := g.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return w, err
	}
	actual, err := w.git(ctx, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return w, err
	}
	if common != actual {
		return w, errors.New("scan worktree belongs to another repository")
	}
	return w, w.verify(ctx, s)
}
func (g checkout) verify(ctx context.Context, s *Scan) error {
	root, err := g.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if root != s.Directory {
		return errors.New("scan directory is not its worktree root")
	}
	head, err := g.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != s.Commit {
		return errors.New("agent changed the scan commit")
	}
	// Research may add untracked files, but edits to tracked code invalidate the evidence.
	if _, err := g.git(ctx, "diff", "--exit-code", "HEAD", "--"); err != nil {
		return fmt.Errorf("scan modified tracked source: %w", err)
	}
	return nil
}
func (g checkout) verifyFiles(ctx context.Context, f Finding) error {
	for _, p := range f.Files {
		if _, err := g.git(ctx, "cat-file", "-e", "HEAD:"+p); err != nil {
			return fmt.Errorf("finding source %q does not exist at scanned commit: %w", p, err)
		}
	}
	return nil
}
func (g checkout) open(ctx context.Context) error {
	if _, err := os.Stat(g.config.Directory); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(g.config.Directory), 0700); err != nil {
			return err
		}
		if _, err := osrun.Run(ctx, "", map[string]string{"GIT_TERMINAL_PROMPT": "0"}, "git", "clone", "--branch", g.config.Branch, "--", g.config.Remote, g.config.Directory); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	root, err := g.git(ctx, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	if root != g.config.Directory {
		return errors.New("directory must be the root of a managed clone")
	}
	remote, err := g.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return err
	}
	if remote != g.config.Remote {
		return errors.New("managed clone origin differs from configuration")
	}
	if _, err := g.git(ctx, "check-ref-format", "refs/heads/"+g.config.Branch); err != nil {
		return err
	}
	_, err = g.git(ctx, "fetch", "--prune", "origin", "+refs/heads/"+g.config.Branch+":refs/remotes/origin/"+g.config.Branch)
	return err
}
