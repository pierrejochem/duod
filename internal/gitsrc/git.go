// Package gitsrc mirrors a Git repository and materializes a revision into a
// slot directory, giving every slot an immutable copy of "its" commit.
package gitsrc

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pierrejochem/duod/internal/runner"
)

// Repo is a bare mirror of a remote repository.
type Repo struct {
	URL    string
	Dir    string // local mirror directory
	Runner runner.Runner
}

// Commit is a resolved revision.
type Commit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// Short returns the abbreviated SHA.
func (c Commit) Short() string {
	if len(c.SHA) > 8 {
		return c.SHA[:8]
	}
	return c.SHA
}

func (r *Repo) git(ctx context.Context, args ...string) ([]byte, error) {
	return r.Runner.Run(ctx, runner.Cmd{
		Name: "git",
		Args: append([]string{"-C", r.Dir}, args...),
		Env:  []string{"GIT_TERMINAL_PROMPT=0"},
	})
}

// Fetch clones the mirror on first use and fetches all refs afterwards.
func (r *Repo) Fetch(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(r.Dir, "HEAD")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(filepath.Dir(r.Dir), 0o755); err != nil {
			return err
		}
		_, err := r.Runner.Run(ctx, runner.Cmd{
			Name: "git",
			Args: []string{"clone", "--mirror", "--quiet", r.URL, r.Dir},
			Env:  []string{"GIT_TERMINAL_PROMPT=0"},
		})
		return err
	}
	_, err := r.git(ctx, "remote", "update", "--prune")
	return err
}

// Resolve turns a branch, tag or SHA into a commit. With a path other than
// ".", the result is the newest commit at or below rev that touched that
// path, so in a monorepo a push to apps/api does not redeploy apps/web.
func (r *Repo) Resolve(ctx context.Context, rev, path string) (Commit, error) {
	if _, err := r.git(ctx, "rev-parse", "--verify", "--quiet", rev+"^{commit}"); err != nil {
		return Commit{}, fmt.Errorf("resolve %q: %w", rev, err)
	}
	args := []string{"log", "-1", "--format=%H%x00%s", rev + "^{commit}"}
	if p := strings.Trim(path, "/"); p != "" && p != "." {
		args = append(args, "--", p)
	}
	out, err := r.git(ctx, args...)
	if err != nil {
		return Commit{}, err
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return Commit{}, fmt.Errorf("no commit at %q touches %q", rev, path)
	}
	sha, subj, _ := strings.Cut(line, "\x00")
	return Commit{SHA: sha, Subject: subj}, nil
}

// Materialize extracts sub-directory `subdir` of commit `sha` into dest,
// replacing whatever was there. The result is dest/<subdir>.
func (r *Repo) Materialize(ctx context.Context, sha, subdir, dest string) (string, error) {
	subdir = path.Clean(strings.TrimPrefix(subdir, "/"))
	args := []string{"archive", "--format=tar", sha}
	if subdir != "." {
		args = append(args, "--", subdir)
	}
	out, err := r.git(ctx, args...)
	if err != nil {
		return "", err
	}
	tmp := dest + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	if err := untar(bytes.NewReader(out), tmp); err != nil {
		_ = os.RemoveAll(tmp)
		return "", err
	}
	if err := os.RemoveAll(dest); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return filepath.Join(dest, filepath.FromSlash(subdir)), nil
}

func untar(rd io.Reader, dest string) error {
	tr := tar.NewReader(rd)
	root, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		target := filepath.Join(root, filepath.FromSlash(h.Name))
		if target != root && !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			return fmt.Errorf("archive entry %q escapes destination", h.Name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(h.Mode)&0o777)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, tr); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			if filepath.IsAbs(h.Linkname) || strings.Contains(h.Linkname, "..") {
				return fmt.Errorf("refusing symlink %q -> %q", h.Name, h.Linkname)
			}
			if err := os.Symlink(h.Linkname, target); err != nil {
				return err
			}
		}
	}
}
