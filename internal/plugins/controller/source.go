package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Limits bound what the controller will accept from a repository.
type Limits struct {
	MaxSourceBytes int64
	MaxFiles       int
}

var (
	githubRE   = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}?(\.git)?$`)
	revisionRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	commitRE   = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// gitEnv gives git a clean, non-interactive environment: no credentials, no
// user or system configuration, no terminal prompts.
func gitEnv(home string, allowLocal bool) []string {
	globalCfg := "/dev/null"
	if allowLocal {
		globalCfg = localGitConfig()
	}
	env := []string{
		"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_GLOBAL=" + globalCfg, "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false",
	}
	return env
}

var (
	localCfgOnce sync.Once
	localCfgPath string
)

// localGitConfig returns a git config file that trusts repositories owned by any
// user. It is used ONLY when local repositories are allowed (development and
// tests): a bind-mounted checkout is owned by the host user, and git refuses it
// otherwise. It must be a file: git's upload-pack subprocess ignores the -c flag and
// GIT_CONFIG_COUNT for safe.directory but honours the global file.
func localGitConfig() string {
	localCfgOnce.Do(func() {
		f, err := os.CreateTemp("", "llmcache-gitconfig-*")
		if err != nil {
			localCfgPath = "/dev/null"
			return
		}
		defer f.Close()
		_, _ = f.WriteString("[safe]\n\tdirectory = *\n")
		localCfgPath = f.Name()
	})
	return localCfgPath
}

func runGit(ctx context.Context, dir string, allowLocal bool, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(dir, allowLocal)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, tail(errb.String(), 300))
	}
	return strings.TrimSpace(out.String()), nil
}

// ValidateRepoURL accepts only https://github.com/<owner>/<repo>. Local paths
// are accepted only with allowLocal, which exists for tests and development.
func ValidateRepoURL(u string, allowLocal bool) error {
	if githubRE.MatchString(u) {
		return nil
	}
	if allowLocal && filepath.IsAbs(u) && !strings.ContainsAny(u, "\x00\n") {
		return nil
	}
	return errors.New("repository must be an https://github.com/<owner>/<repo> URL")
}

// FetchRepo checks out exactly one revision into dest (which must not exist) and
// returns the resolved 40-hex commit. History, submodules, hooks and the .git
// directory are not kept, and symlinks are checked out as plain files so a
// hostile repository cannot point the build at host paths.
func FetchRepo(ctx context.Context, repoURL, revision, dest string, allowLocal bool) (string, error) {
	if err := ValidateRepoURL(repoURL, allowLocal); err != nil {
		return "", err
	}
	if revision == "" {
		revision = "HEAD"
	}
	if revision != "HEAD" && !revisionRE.MatchString(revision) || strings.Contains(revision, "..") {
		return "", errors.New("invalid revision")
	}
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	steps := [][]string{
		{"init", "--quiet"},
		{"config", "core.symlinks", "false"},
		{"config", "core.hooksPath", "/dev/null"},
		{"remote", "add", "origin", repoURL},
		{"fetch", "--quiet", "--depth", "1", "--no-tags", "--no-recurse-submodules", "origin", revision},
		{"-c", "advice.detachedHead=false", "checkout", "--quiet", "--detach", "FETCH_HEAD"},
	}
	for _, s := range steps {
		if _, err := runGit(ctx, dest, allowLocal, s...); err != nil {
			return "", err
		}
	}
	commit, err := runGit(ctx, dest, allowLocal, "rev-parse", "HEAD")
	if err != nil || !commitRE.MatchString(commit) {
		return "", errors.New("could not resolve the revision to a commit")
	}
	if err := os.RemoveAll(filepath.Join(dest, ".git")); err != nil {
		return "", err
	}
	return commit, nil
}

// CheckTree walks a checked-out (or copied) tree, enforcing size and file-count
// limits and refusing symlinks and special files. It returns a SHA-256 over
// every file's path and content, recorded as build provenance.
func CheckTree(root string, lim Limits) (string, error) {
	type entry struct{ rel, sum string }
	var entries []entry
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("%s is a symlink; symlinks are not allowed in plugin sources", rel)
		case info.IsDir():
			return nil
		case !info.Mode().IsRegular():
			return fmt.Errorf("%s is not a regular file", rel)
		}
		total += info.Size()
		if total > lim.MaxSourceBytes {
			return fmt.Errorf("source exceeds the %d byte limit", lim.MaxSourceBytes)
		}
		if len(entries) >= lim.MaxFiles {
			return fmt.Errorf("source has more than %d files", lim.MaxFiles)
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		entries = append(entries, entry{filepath.ToSlash(rel), hex.EncodeToString(h.Sum(nil))})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%s\n", e.rel, e.sum)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// SafeJoin resolves rel inside root and refuses anything that is not a plain
// path within it (no '..', no symlink anywhere along the way).
func SafeJoin(root, rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) || strings.Contains(rel, "\x00") {
		return "", fmt.Errorf("invalid path %q", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the repository", rel)
	}
	cur := root
	if clean != "." {
		for _, part := range strings.Split(clean, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			info, err := os.Lstat(cur)
			if err != nil {
				return "", fmt.Errorf("path %q does not exist in the repository", rel)
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("path %q goes through a symlink", rel)
			}
		}
	}
	return cur, nil
}

// ReadRegular reads a small regular file inside root.
func ReadRegular(root, rel string, max int64) ([]byte, error) {
	p, err := SafeJoin(root, rel)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(p)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is missing or not a regular file", rel)
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("%s is larger than %d bytes", rel, max)
	}
	return data, nil
}

// copyTree copies src into dst (created), refusing symlinks, with the same
// limits as CheckTree.
func copyTree(src, dst string, lim Limits) error {
	var total int64
	files := 0
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink", rel)
		}
		if info.IsDir() {
			if d.Name() == "__pycache__" || d.Name() == ".git" {
				return fs.SkipDir
			}
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		total += info.Size()
		files++
		if total > lim.MaxSourceBytes || files > lim.MaxFiles {
			return errors.New("plugin package exceeds the source limits")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		if cerr := out.Close(); err == nil {
			err = cerr
		}
		return err
	})
}
