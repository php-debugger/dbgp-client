package dbgp

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// PathMapping maps a local directory to the directory the engine sees, for
// PHP running in a container or on a remote server.
type PathMapping struct {
	Local  string // local directory, e.g. /home/me/project
	Remote string // the same directory on the engine's side, e.g. /var/www/html
}

// PathMap translates file paths between this machine and the engine. Each
// mapping replaces a directory prefix; the longest matching one wins, and
// paths no mapping matches are left as they are.
//
// Remote paths use forward slashes, with Windows drives as "C:/dir", so a
// Linux or Windows server can be debugged from any local OS.
type PathMap []PathMapping

// newPathMap validates and normalises mappings.
func newPathMap(mappings []PathMapping) (PathMap, error) {
	m := make(PathMap, 0, len(mappings))
	for _, pm := range mappings {
		if pm.Local == "" || pm.Remote == "" {
			return nil, fmt.Errorf("path mapping %+v: local and remote must both be set", pm)
		}
		m = append(m, PathMapping{
			Local:  filepath.Clean(pm.Local),
			Remote: cleanRemote(pm.Remote),
		})
	}
	return m, nil
}

// cleanRemote normalises a remote path to forward slashes without a
// trailing slash (except for a root).
func cleanRemote(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	for len(p) > 1 && strings.HasSuffix(p, "/") && !strings.HasSuffix(p, ":/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

// ToRemote translates a local path to the engine's path.
func (m PathMap) ToRemote(local string) string {
	local = filepath.Clean(local)
	best, rest := -1, ""
	for i, pm := range m {
		if r, ok := cutPathPrefix(local, pm.Local, string(filepath.Separator)); ok && (best < 0 || len(pm.Local) > len(m[best].Local)) {
			best, rest = i, r
		}
	}
	if best < 0 {
		return local
	}
	return joinPath(m[best].Remote, filepath.ToSlash(rest), "/")
}

// ToLocal translates a path from the engine to a local path.
func (m PathMap) ToLocal(remote string) string {
	remote = cleanRemote(remote)
	best, rest := -1, ""
	for i, pm := range m {
		if r, ok := cutPathPrefix(remote, pm.Remote, "/"); ok && (best < 0 || len(pm.Remote) > len(m[best].Remote)) {
			best, rest = i, r
		}
	}
	if best < 0 {
		return filepath.FromSlash(remote)
	}
	return joinPath(m[best].Local, filepath.FromSlash(rest), string(filepath.Separator))
}

// cutPathPrefix reports whether p is prefix or inside it, and returns the
// remainder. Matching is by whole path components: /var/www does not
// contain /var/www2.
func cutPathPrefix(p, prefix, sep string) (string, bool) {
	if p == prefix {
		return "", true
	}
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	if rest, ok := strings.CutPrefix(p, prefix); ok {
		return rest, true
	}
	return "", false
}

func joinPath(dir, rest, sep string) string {
	if rest == "" {
		return dir
	}
	if strings.HasSuffix(dir, sep) {
		return dir + rest
	}
	return dir + sep + rest
}

// engineURI returns the file URI the engine uses for a local path.
func (m PathMap) engineURI(local string) string {
	if strings.HasPrefix(local, "file://") {
		return MakeFileURI(local)
	}
	return MakeFileURI(m.ToRemote(local))
}

// localPath returns the local path for a file URI from the engine. URIs
// that are not file URIs, such as dbgp://1 for eval'd code, are returned
// unchanged.
func (m PathMap) localPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return uri
	}
	remote := u.Path
	if len(remote) >= 3 && remote[0] == '/' && remote[2] == ':' {
		remote = remote[1:] // /C:/dir -> C:/dir
	}
	if u.Host != "" {
		remote = "//" + u.Host + remote
	}
	return m.ToLocal(remote)
}
