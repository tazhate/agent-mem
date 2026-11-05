// Package project maps a working directory to a stable project identity. All worktrees of one
// repository share the git common dir, so they share one project and one memory.
package project

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Info struct {
	Key  string // realpath of the git common dir, or of the cwd outside git
	Name string // human name: repository directory name
	Root string // worktree top level the cwd belongs to
	Git  bool
}

// Resolver caches lookups; the daemon resolves the same few cwds over and over.
type Resolver struct {
	mu    sync.Mutex
	cache map[string]Info
}

func NewResolver() *Resolver { return &Resolver{cache: map[string]Info{}} }

func (r *Resolver) Resolve(cwd string) Info {
	if cwd == "" {
		return Info{Key: "unknown", Name: "unknown"}
	}
	r.mu.Lock()
	info, ok := r.cache[cwd]
	r.mu.Unlock()
	if ok {
		return info
	}
	info = Resolve(cwd)
	r.mu.Lock()
	r.cache[cwd] = info
	r.mu.Unlock()
	return info
}

// Resolve walks up from cwd looking for .git without executing git.
func Resolve(cwd string) Info {
	abs, err := filepath.Abs(cwd)
	if err != nil {
		abs = cwd
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		abs = real
	}
	for dir := abs; ; dir = filepath.Dir(dir) {
		gitPath := filepath.Join(dir, ".git")
		if st, err := os.Stat(gitPath); err == nil {
			var common string
			if st.IsDir() {
				common = gitPath
			} else {
				common = commonDirFromFile(gitPath, dir)
			}
			if common != "" {
				return Info{Key: common, Name: nameFromCommon(common), Root: dir, Git: true}
			}
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	return Info{Key: abs, Name: filepath.Base(abs), Root: abs}
}

// commonDirFromFile follows a ".git" file ("gitdir: <path>") of a worktree or submodule.
func commonDirFromFile(gitFile, dir string) string {
	f, err := os.Open(gitFile)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return ""
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "gitdir:")
	if !ok {
		return ""
	}
	gitdir = strings.TrimSpace(gitdir)
	if !filepath.IsAbs(gitdir) {
		gitdir = filepath.Join(dir, gitdir)
	}
	// Linked worktrees store the path to the shared repository in "commondir".
	if b, err := os.ReadFile(filepath.Join(gitdir, "commondir")); err == nil {
		c := strings.TrimSpace(string(b))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gitdir, c)
		}
		gitdir = c
	}
	gitdir = filepath.Clean(gitdir)
	if real, err := filepath.EvalSymlinks(gitdir); err == nil {
		gitdir = real
	}
	return gitdir
}

func nameFromCommon(common string) string {
	base := filepath.Base(common)
	if base == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	// Submodules live under <super>/.git/modules/<name>; bare repos are "<name>.git".
	if strings.Contains(common, string(filepath.Separator)+"modules"+string(filepath.Separator)) {
		return base
	}
	return strings.TrimSuffix(base, ".git")
}
