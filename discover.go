package featurebot

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/BrokkAi/feature-bot/internal/osrun"
)

// Discover configures a managed checkout from a working tree (including a
// subdirectory), a bare repository, or a Git URL. It does not edit the source.
func Discover(ctx context.Context, target, branch string) (Config, error) {
	cfg := DefaultConfig()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if target == "" {
		target = "."
	}
	var root, remoteName string
	if info, err := os.Stat(target); err == nil && info.IsDir() {
		absolute, err := filepath.Abs(target)
		if err != nil {
			return cfg, err
		}
		git := func(args ...string) (string, error) {
			return osrun.Run(ctx, absolute, map[string]string{"GIT_TERMINAL_PROMPT": "0"}, append([]string{"git"}, args...)...)
		}
		bare, err := git("rev-parse", "--is-bare-repository")
		if err != nil {
			return cfg, errors.New("not inside a Git repository; run feature-bot in a checkout or pass a repository URL")
		}
		if bare == "true" {
			cfg.Remote, err = canonical(absolute)
			if err != nil {
				return cfg, err
			}
		} else {
			root, err = git("rev-parse", "--show-toplevel")
			if err != nil {
				return cfg, err
			}
			remotes, err := git("remote")
			if err != nil {
				return cfg, err
			}
			for _, name := range strings.Fields(remotes) {
				if name == "origin" {
					remoteName = name
				}
			}
			if remoteName == "" {
				names := strings.Fields(remotes)
				if len(names) == 0 {
					return cfg, errors.New("this repository has no remote; add origin or pass a repository URL to feature-bot")
				}
				if len(names) != 1 {
					return cfg, errors.New("this repository has several remotes and no origin; pass the issue repository URL explicitly")
				}
				remoteName = names[0]
			}
			cfg.Remote, err = git("remote", "get-url", remoteName)
			if err != nil {
				return cfg, err
			}
			// Git resolves relative filesystem remotes from the repository root.
			if !isRemoteURL(cfg.Remote) && !filepath.IsAbs(cfg.Remote) {
				cfg.Remote = filepath.Join(root, cfg.Remote)
			}
			if branch == "" {
				ref, _ := git("symbolic-ref", "--quiet", "refs/remotes/"+remoteName+"/HEAD")
				branch = strings.TrimPrefix(ref, "refs/remotes/"+remoteName+"/")
			}
		}
	} else {
		if !isRemoteURL(target) {
			return cfg, fmt.Errorf("repository directory %q does not exist; pass a checkout directory or Git URL", target)
		}
		cfg.Remote = target
	}
	if !isRemoteURL(cfg.Remote) {
		resolved, err := canonical(cfg.Remote)
		if err != nil {
			return cfg, err
		}
		cfg.Remote = resolved
	}
	if branch == "" {
		refs, err := osrun.Run(ctx, "", map[string]string{"GIT_TERMINAL_PROMPT": "0"}, "git", "ls-remote", "--symref", "--", cfg.Remote, "HEAD")
		if err != nil {
			return cfg, fmt.Errorf("cannot read repository's default branch; check Git access or use --branch: %w", err)
		}
		for _, line := range strings.Split(refs, "\n") {
			fields := strings.Fields(line)
			if len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD" {
				branch = strings.TrimPrefix(fields[1], "refs/heads/")
				break
			}
		}
		if branch == "" {
			return cfg, errors.New("repository has no advertised default branch; use --branch NAME")
		}
	}
	cfg.Branch = branch
	base, err := stateHome()
	if err != nil {
		return cfg, err
	}
	identity := sha256.Sum256([]byte(cfg.Remote + "\x00" + cfg.Branch))
	name := strings.TrimSuffix(filepath.Base(cfg.Remote), ".git")
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, name)
	if len(name) > 48 {
		name = name[:48]
	}
	workspace := filepath.Join(base, "feature-bot", fmt.Sprintf("%s-%x", name, identity[:12]))
	cfg.Directory, err = canonical(filepath.Join(workspace, "checkout"))
	if err != nil {
		return cfg, err
	}
	cfg.StateDirectory, err = canonical(filepath.Join(workspace, "state"))
	if err != nil {
		return cfg, err
	}
	return cfg, cfg.Validate()
}

func isRemoteURL(value string) bool {
	if u, err := url.Parse(value); err == nil && u.Scheme != "" {
		return true
	}
	return strings.Contains(value, ":") && !filepath.IsAbs(value)
}
func stateHome() (string, error) {
	if value := os.Getenv("XDG_STATE_HOME"); value != "" {
		if !filepath.IsAbs(value) {
			return "", errors.New("XDG_STATE_HOME must be absolute")
		}
		return value, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state"), nil
}

// ResolveAgent uses an installed adapter when available and otherwise lets npx
// provision the default Codex adapter. Explicit commands are never replaced.
func ResolveAgent(cfg *Config, automatic bool) error {
	if len(cfg.Agent.Command) == 0 {
		return errors.New("agent command is empty")
	}
	if _, err := exec.LookPath(cfg.Agent.Command[0]); err == nil {
		return nil
	}
	if automatic && cfg.Agent.Command[0] == "codex-acp" {
		if _, err := exec.LookPath("npx"); err == nil {
			cfg.Agent.Command = append([]string{"npx", "--yes", "@agentclientprotocol/codex-acp"}, cfg.Agent.Command[1:]...)
			return nil
		}
		return errors.New("Codex needs codex-acp or Node.js (npx); install either, or select an installed ACP agent with --agent")
	}
	return fmt.Errorf("agent executable %q was not found; use --agent with an installed ACP agent", cfg.Agent.Command[0])
}
