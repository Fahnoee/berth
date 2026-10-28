// Package gitwt resolves which git worktree (and branch) a directory belongs to
// without shelling out to git.
package gitwt

import (
	"os"
	"path/filepath"
	"strings"
)

// Info describes the worktree a path lives in.
type Info struct {
	Root   string // worktree root (directory containing .git), or the input dir if not in a repo
	Branch string // current branch, short sha for detached HEAD, "" if not a repo
	IsRepo bool
}

// Resolve walks up from dir looking for a .git file or directory.
func Resolve(dir string) Info {
	dir = filepath.Clean(dir)
	for d := dir; ; {
		gitPath := filepath.Join(d, ".git")
		if fi, err := os.Stat(gitPath); err == nil {
			gitDir := gitPath
			if !fi.IsDir() {
				gitDir = readGitFile(gitPath, d)
			}
			return Info{Root: d, Branch: readBranch(gitDir), IsRepo: true}
		}
		parent := filepath.Dir(d)
		if parent == d {
			break
		}
		d = parent
	}
	return Info{Root: dir}
}

// Cwd resolves the worktree of the current working directory.
func Cwd() Info {
	wd, err := os.Getwd()
	if err != nil {
		return Info{}
	}
	if real, err := filepath.EvalSymlinks(wd); err == nil {
		wd = real
	}
	return Resolve(wd)
}

func readGitFile(path, base string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	p, ok := strings.CutPrefix(line, "gitdir:")
	if !ok {
		return ""
	}
	p = strings.TrimSpace(p)
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return p
}

func readBranch(gitDir string) string {
	if gitDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return ""
	}
	head := strings.TrimSpace(string(b))
	if ref, ok := strings.CutPrefix(head, "ref: "); ok {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	if len(head) >= 7 {
		return head[:7]
	}
	return head
}

// Exists reports whether a worktree root still exists on disk.
func Exists(root string) bool {
	if root == "" {
		return true
	}
	_, err := os.Stat(root)
	return err == nil
}
