package featurebot

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BrokkAi/acp-go/runner"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(raw []byte) error {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(value)
	if err == nil {
		*d = Duration(parsed)
	}
	return err
}
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }

type AgentConfig = runner.AgentConfig
type GitHubConfig struct {
	Repo string `json:"repo,omitempty"`
	Host string `json:"host"`
}
type Config struct {
	Remote           string       `json:"remote"`
	Branch           string       `json:"branch"`
	Directory        string       `json:"directory"`
	StateDirectory   string       `json:"state_directory"`
	InstructionFiles []string     `json:"instruction_files"`
	Agent            AgentConfig  `json:"agent"`
	GitHub           GitHubConfig `json:"github"`
	Poll             Duration     `json:"poll"`
	Timeout          Duration     `json:"timeout"`

	RetryDelay Duration `json:"retry_delay"`
	Attempts   int      `json:"attempts"`
	Labels     []string `json:"labels,omitempty"`

	MaxIssues int      `json:"max_issues"`
	DryRun    bool     `json:"dry_run"`
	Focus     string   `json:"focus,omitempty"`
	Verify    []string `json:"verify,omitempty"`
}

func DefaultConfig() Config {
	return Config{Branch: "master", Directory: "var/checkout", StateDirectory: "var/state",
		InstructionFiles: []string{"AGENTS.md", "CONTRIBUTING.md", "README.md"},
		Agent:            AgentConfig{Command: []string{"codex-acp"}}, GitHub: GitHubConfig{Host: "github.com"},
		Poll: Duration(30 * time.Minute), Timeout: Duration(2 * time.Hour), RetryDelay: Duration(15 * time.Minute), Attempts: 3,
		MaxIssues: 3}
}
func ReadConfig(filename string) (Config, error) {
	cfg := DefaultConfig()
	f, err := os.Open(filename)
	if err != nil {
		return cfg, err
	}
	defer f.Close()
	decoder := json.NewDecoder(f)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return cfg, errors.New("expected one configuration object")
	}
	base, err := filepath.Abs(filepath.Dir(filename))
	if err != nil {
		return cfg, err
	}
	for _, path := range []*string{&cfg.Directory, &cfg.StateDirectory} {
		if !filepath.IsAbs(*path) {
			*path = filepath.Join(base, *path)
		}
		*path, err = canonical(*path)
		if err != nil {
			return cfg, err
		}
	}
	return cfg, cfg.Validate()
}

// Resolve existing ancestors, including symlinks, even before clone creation.
func canonical(path string) (string, error) { return resolvePath(filepath.Clean(path), 0) }
func resolvePath(path string, depth int) (string, error) {
	if depth > 255 {
		return "", errors.New("too many symlink levels")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	// EvalSymlinks cannot resolve a dangling link. Resolve its target explicitly
	// so future directories cannot make previously separate paths overlap.
	info, statErr := os.Lstat(path)
	if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(path), target)
		}
		return resolvePath(target, depth+1)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	parent, err = resolvePath(parent, depth+1)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(path)), nil
}

var slug = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func (c Config) GitHubRepo() string {
	if c.GitHub.Repo != "" {
		return c.GitHub.Repo
	}
	path := ""
	if prefix := "git@" + c.GitHub.Host + ":"; strings.HasPrefix(c.Remote, prefix) {
		path = strings.TrimPrefix(c.Remote, prefix)
	}
	if u, err := url.Parse(c.Remote); err == nil && u.Hostname() == c.GitHub.Host {
		path = strings.TrimPrefix(u.Path, "/")
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if slug.MatchString(path) {
		return path
	}
	return ""
}
func (c Config) Validate() error {
	if c.Remote == "" || strings.HasPrefix(c.Remote, "-") {
		return errors.New("remote is required")
	}
	if c.Branch == "" || strings.HasPrefix(c.Branch, "-") || strings.Contains(c.Branch, "..") {
		return errors.New("invalid branch")
	}
	if c.Directory == "" || c.StateDirectory == "" {
		return errors.New("directory and state_directory are required")
	}
	for _, pair := range [][2]string{{c.Directory, c.StateDirectory}, {c.StateDirectory, c.Directory}, {c.Directory + "-scans", c.StateDirectory}, {c.StateDirectory, c.Directory + "-scans"}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err != nil {
			return err
		}
		if rel == "." || filepath.IsLocal(rel) {
			return errors.New("checkout and state directories must not overlap")
		}
	}
	if len(c.Agent.Command) == 0 || c.Agent.Command[0] == "" {
		return errors.New("agent.command is required")
	}
	if c.Agent.Effort != "" && strings.TrimSpace(c.Agent.Effort) == "" {
		return errors.New("agent.effort requires a reasoning effort value")
	}
	if c.GitHub.Repo != "" && !slug.MatchString(c.GitHub.Repo) {
		return errors.New("github.repo must be owner/repository")
	}
	if c.GitHub.Host == "" || strings.ContainsAny(c.GitHub.Host, " /:\\") {
		return errors.New("invalid github.host")
	}
	if len(c.Verify) > 0 && c.Verify[0] == "" {
		return errors.New("verify command is empty")
	}
	for _, name := range c.InstructionFiles {
		if !filepath.IsLocal(name) {
			return fmt.Errorf("instruction file must be relative: %s", name)
		}
	}

	if c.Poll <= 0 || c.Timeout <= 0 || c.RetryDelay <= 0 || c.Attempts < 1 || c.MaxIssues < 1 || c.MaxIssues > 20 {
		return errors.New("durations and attempts must be positive; max_issues must be between 1 and 20")
	}
	return nil
}
