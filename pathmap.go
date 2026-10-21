package dbgp

import (
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
)

// localIgnoresCase reports whether local paths are compared regardless of
// case: on Windows, and on macOS, whose disks ignore case unless they were
// formatted otherwise. It is a variable so tests can change it.
var localIgnoresCase = runtime.GOOS == "windows" || runtime.GOOS == "darwin"

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
		if r, ok := cutPathPrefix(local, pm.Local, string(filepath.Separator), localIgnoresCase); ok && (best < 0 || len(pm.Local) > len(m[best].Local)) {
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
		if r, ok := cutPathPrefix(remote, pm.Remote, "/", false); ok && (best < 0 || len(pm.Remote) > len(m[best].Remote)) {
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
// contain /var/www2. It ignores case if ignoreCase is set, and for a Windows
// prefix, as Windows does: C:\Project contains c:\project\a.php.
func cutPathPrefix(p, prefix, sep string, ignoreCase bool) (string, bool) {
	if samePath(p, prefix, ignoreCase) {
		return "", true
	}
	if !strings.HasSuffix(prefix, sep) {
		prefix += sep
	}
	if len(p) < len(prefix) || !samePath(p[:len(prefix)], prefix, ignoreCase) {
		return "", false
	}
	return p[len(prefix):], true
}

// samePath reports whether paths a and b are the same: regardless of case
// if ignoreCase is set or for Windows paths, exactly otherwise.
func samePath(a, b string, ignoreCase bool) bool {
	if ignoreCase || windowsPath(a) || windowsPath(b) {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// windowsPath reports whether p is a Windows path, which Windows compares
// regardless of case: one that starts with a drive, such as C:\dir or
// C:/dir, or a UNC share, such as \\server\share or //server/share.
func windowsPath(p string) bool {
	if len(p) >= 2 && p[1] == ':' && 'a' <= p[0]|0x20 && p[0]|0x20 <= 'z' {
		return true
	}
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, "//")
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
