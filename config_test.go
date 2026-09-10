package featurebot

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStrictConfigAndPaths(t *testing.T) {
	p := filepath.Join(canonicalTestDir(t), "config.json")
	for _, raw := range []string{
		`{"remote":"https://github.com/o/r.git","unknown":true}`,
		`{"remote":"https://github.com/o/r.git"} {}`,
		`{"remote":"https://github.com/o/r.git","max_issues":0}`,
		`{"remote":"https://github.com/o/r.git","max_issues":21}`,
		`{"remote":"https://github.com/o/r.git","poll":"0s"}`,
		`{"remote":"https://github.com/o/r.git","directory":"checkout","state_directory":"checkout-scans/sub"}`,
	} {
		writeTestFile(t, p, raw)
		if _, err := ReadConfig(p); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	writeTestFile(t, p, `{"remote":"https://github.com/o/r.git"}`)
	c, err := ReadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHubRepo() != "o/r" || !filepath.IsAbs(c.Directory) || c.MaxIssues != 3 || c.DryRun {
		t.Fatalf("bad defaults %+v", c)
	}
	if err := os.Symlink("checkout", filepath.Join(filepath.Dir(p), "alias")); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, p, `{"remote":"https://github.com/o/r.git","directory":"checkout","state_directory":"alias/state"}`)
	if _, err := ReadConfig(p); err == nil {
		t.Fatal("dangling symlink overlap accepted")
	}
}
func TestRepositoryLockAcrossBranches(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", canonicalTestDir(t))
	c := DefaultConfig()
	c.Remote = "https://github.com/o/r.git"
	dir := canonicalTestDir(t)
	c.Directory = filepath.Join(dir, "a")
	c.StateDirectory = filepath.Join(dir, "a-state")
	unlock, err := lockConfig(c)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	c.Branch = "other"
	c.Directory = filepath.Join(dir, "b")
	c.StateDirectory = filepath.Join(dir, "b-state")
	if release, err := lockConfig(c); err == nil {
		release()
		t.Fatal("same repository can run concurrently across branches")
	}
}
