// Package discovery walks configured roots to find git repos with GitHub remotes.
package discovery

import (
	"bufio"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ionrock/aupr/internal/execx"
)

// Repo is a discovered git repository with a resolved GitHub remote.
type Repo struct {
	Path    string // absolute path on disk
	Remote  string // the raw remote URL
	Owner   string // GitHub owner
	Name    string // GitHub repo name
	NWO     string // "owner/name"
	Default string // default remote name (usually "origin")
}

// Walker discovers repos by walking filesystem roots.
type Walker struct {
	Runner execx.Runner
	Logger *slog.Logger
}

// Walk walks each root, finds `.git` entries, and resolves GitHub remotes.
// Roots that don't exist are skipped with a debug log.
func (w *Walker) Walk(ctx context.Context, roots []string) ([]Repo, error) {
	var out []Repo
	seen := map[string]struct{}{}

	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			if w.Logger != nil {
				w.Logger.Debug("skipping root", "root", root, "err", err)
			}
			continue
		}
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if werr != nil {
				// Permission errors etc — skip the subtree.
				return filepath.SkipDir
			}
			if !d.IsDir() {
				return nil
			}
			name := d.Name()
			// Prune big noisy directories.
			if name == "node_modules" || name == ".venv" || name == "target" || name == "vendor" || name == ".tox" {
				return filepath.SkipDir
			}
			// A repo is any dir containing .git (file or dir — worktrees use a file).
			gitPath := filepath.Join(path, ".git")
			if _, err := os.Stat(gitPath); err != nil {
				return nil
			}
			abs, err := filepath.Abs(path)
			if err != nil {
				return nil
			}
			if _, dup := seen[abs]; dup {
				return filepath.SkipDir
			}
			seen[abs] = struct{}{}
			repo, ok := w.resolveRemote(abs)
			if ok {
				out = append(out, repo)
			} else if w.Logger != nil {
				w.Logger.Debug("no github remote", "path", abs)
			}
			// Don't descend into a repo's own subdirs looking for more repos —
			// but DO keep walking siblings.
			return filepath.SkipDir
		})
		if err != nil && err != context.Canceled {
			if w.Logger != nil {
				w.Logger.Warn("walk error", "root", root, "err", err)
			}
		}
	}
	return out, nil
}

// resolveRemote parses .git/config directly. This avoids a subprocess per repo
// and works for both normal repos and worktrees.
func (w *Walker) resolveRemote(repoPath string) (Repo, bool) {
	// For linked worktrees, .git is a file pointing at the gitdir.
	gitDir := filepath.Join(repoPath, ".git")
	if info, err := os.Stat(gitDir); err == nil && !info.IsDir() {
		// Worktree: the shared config lives in commondir.
		// We attempt to resolve by reading the gitdir pointer.
		if cd, ok := commonDir(gitDir); ok {
			gitDir = cd
		}
	}
	cfg := filepath.Join(gitDir, "config")
	f, err := os.Open(cfg)
	if err != nil {
		return Repo{}, false
	}
	defer f.Close()

	remotes := map[string]string{}
	var current string
	sc := bufio.NewScanner(f)
	remoteHeader := regexp.MustCompile(`^\[remote "([^"]+)"\]$`)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if m := remoteHeader.FindStringSubmatch(line); m != nil {
			current = m[1]
			continue
		}
		if strings.HasPrefix(line, "[") {
			current = ""
			continue
		}
		if current != "" && strings.HasPrefix(line, "url") {
			if eq := strings.Index(line, "="); eq != -1 {
				remotes[current] = strings.TrimSpace(line[eq+1:])
			}
		}
	}
	// Prefer origin, fall back to the first remote.
	preferred := "origin"
	url, ok := remotes[preferred]
	if !ok {
		for k, v := range remotes {
			preferred = k
			url = v
			ok = true
			break
		}
	}
	if !ok {
		return Repo{}, false
	}
	owner, name, ok := parseGitHub(url)
	if !ok {
		return Repo{}, false
	}
	return Repo{
		Path:    repoPath,
		Remote:  url,
		Owner:   owner,
		Name:    name,
		NWO:     owner + "/" + name,
		Default: preferred,
	}, true
}

// commonDir resolves a worktree .git pointer file to the shared gitdir.
// Returns (commondir, true) if resolvable.
func commonDir(gitFile string) (string, bool) {
	b, err := os.ReadFile(gitFile)
	if err != nil {
		return "", false
	}
	line := strings.TrimSpace(string(b))
	const prefix = "gitdir:"
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	wtGitDir := strings.TrimSpace(strings.TrimPrefix(line, prefix))
	if !filepath.IsAbs(wtGitDir) {
		wtGitDir = filepath.Join(filepath.Dir(gitFile), wtGitDir)
	}
	// Look for commondir file inside the worktree-specific gitdir.
	cd, err := os.ReadFile(filepath.Join(wtGitDir, "commondir"))
	if err != nil {
		return wtGitDir, true
	}
	rel := strings.TrimSpace(string(cd))
	if filepath.IsAbs(rel) {
		return rel, true
	}
	return filepath.Join(wtGitDir, rel), true
}

// parseGitHub extracts owner/name from common GitHub URL forms:
//
//	https://github.com/owner/name(.git)?
//	git@github.com:owner/name(.git)?
//	ssh://git@github.com/owner/name(.git)?
func parseGitHub(url string) (owner, name string, ok bool) {
	u := strings.TrimSpace(url)
	u = strings.TrimSuffix(u, ".git")
	var rest string
	switch {
	case strings.HasPrefix(u, "git@github.com:"):
		rest = strings.TrimPrefix(u, "git@github.com:")
	case strings.HasPrefix(u, "ssh://git@github.com/"):
		rest = strings.TrimPrefix(u, "ssh://git@github.com/")
	case strings.HasPrefix(u, "https://github.com/"):
		rest = strings.TrimPrefix(u, "https://github.com/")
	case strings.HasPrefix(u, "http://github.com/"):
		rest = strings.TrimPrefix(u, "http://github.com/")
	default:
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}
