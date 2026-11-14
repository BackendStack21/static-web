package handler

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/url"
	"os"
	"sort"
	"strings"

	"github.com/BackendStack21/static-web/internal/cache"
	"github.com/BackendStack21/static-web/internal/security"
	"github.com/valyala/fasthttp"
)

const modTimeFormat = "2006-01-02 15:04"

// dirEntry holds display data for a single directory entry.
type dirEntry struct {
	Name    string
	HRef    string
	IsDir   bool
	Size    int64
	ModTime string // pre-formatted as "2006-01-02 15:04" UTC, or "" for synthetic entries
}

// dirListData is passed to the directory listing template.
type dirListData struct {
	URLPath     string
	Breadcrumbs []breadcrumb
	Entries     []dirEntry
}

// breadcrumb is a single element in the path navigation bar.
type breadcrumb struct {
	Label string
	HRef  string
}

// dirListStyle is the exact text of the listing's inline <style> element.
// It contains no template actions, so it is served byte for byte and its
// hash (dirListStyleSource) can be computed once.
const dirListStyle = `
    *, *::before, *::after { box-sizing: border-box; }
    body { font-family: ui-monospace, "SFMono-Regular", Consolas, monospace;
           font-size: 14px; margin: 0; padding: 24px 32px;
           background: #0f1117; color: #e2e8f0; }
    h1 { font-size: 16px; font-weight: 600; margin: 0 0 16px;
         color: #94a3b8; letter-spacing: 0.02em; }
    a { color: #60a5fa; text-decoration: none; }
    a:hover { text-decoration: underline; }
    .breadcrumb { margin-bottom: 20px; font-size: 13px; color: #64748b; }
    .breadcrumb a { color: #94a3b8; }
    .breadcrumb span { margin: 0 4px; }
    table { width: 100%; border-collapse: collapse; }
    th { text-align: left; padding: 6px 12px; font-size: 11px; font-weight: 600;
         text-transform: uppercase; letter-spacing: 0.08em; color: #64748b;
         border-bottom: 1px solid #1e293b; }
    td { padding: 5px 12px; border-bottom: 1px solid #1e293b; }
    tr:last-child td { border-bottom: none; }
    tr:hover td { background: #1e293b; }
    .dir { color: #fbbf24; }
    .size { text-align: right; color: #94a3b8; font-size: 13px; }
    .mtime { color: #64748b; font-size: 13px; white-space: nowrap; }
  `

// dirListStyleSource is the CSP source expression ('sha256-…') that allows
// dirListStyle as an inline style (HTTP-F7).
var dirListStyleSource = func() string {
	sum := sha256.Sum256([]byte(dirListStyle))
	return "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
}()

// dirListTemplate is the HTML template for directory listings.
// Parsed once at package initialisation; any syntax errors panic at startup.
var dirListTemplate = template.Must(
	template.New("dirlist").
		Funcs(template.FuncMap{"formatSize": formatSize}).
		Parse(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Index of {{.URLPath}}</title>
  <style>` + dirListStyle + `</style>
</head>
<body>
  <h1>Index of {{.URLPath}}</h1>
  <div class="breadcrumb">
    {{- range $i, $bc := .Breadcrumbs}}
    {{- if $i}}<span>/</span>{{end -}}
    {{- if $bc.HRef}}<a href="{{$bc.HRef}}">{{$bc.Label}}</a>{{else}}{{$bc.Label}}{{end -}}
    {{- end}}
  </div>
  <table>
    <thead>
      <tr>
        <th>Name</th>
        <th>Size</th>
        <th>Modified</th>
      </tr>
    </thead>
    <tbody>
      {{range .Entries -}}
      <tr>
        <td>
          {{- if .IsDir}}<span class="dir">&#128193;</span> <a href="{{.HRef}}">{{.Name}}/</a>
          {{- else}}&#128196; <a href="{{.HRef}}">{{.Name}}</a>{{end -}}
        </td>
        <td class="size">{{if .IsDir}}&mdash;{{else}}{{formatSize .Size}}{{end}}</td>
        <td class="mtime">{{.ModTime}}</td>
      </tr>
      {{end -}}
    </tbody>
  </table>
</body>
</html>
`))

// serveDirectoryListing reads absDir, filters and sorts entries, and renders
// the HTML directory listing to ctx.
// Dotfiles are hidden when cfg.Security.BlockDotfiles is true.
func (h *FileHandler) serveDirectoryListing(ctx *fasthttp.RequestCtx, absDir, urlPath string) {
	entries, err := h.readDirInRoot(absDir)
	if err != nil {
		switch {
		case cache.IsEscape(err), errors.Is(err, cache.ErrFileChanged):
			log.Printf("handler: refusing to list %q: it changed after validation", absDir)
			writeError(ctx, "Forbidden", fasthttp.StatusForbidden)
		case os.IsPermission(err):
			log.Printf("handler: permission denied listing %q (%s)", absDir, rootPermHint)
			writeError(ctx, "Forbidden", fasthttp.StatusForbidden)
		default:
			writeError(ctx, "Internal Server Error", fasthttp.StatusInternalServerError)
		}
		return
	}

	// Normalise URL path so breadcrumb and hrefs are consistent.
	if !strings.HasSuffix(urlPath, "/") {
		urlPath += "/"
	}

	blockDotfiles := h.cfg.Security.BlockDotfiles

	var dirs, files []dirEntry
	for _, e := range entries {
		name := e.Name()
		// Skip dotfiles when configured.
		if blockDotfiles && strings.HasPrefix(name, ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Mode()&os.ModeSymlink != 0 {
			// FS-5: describe a symlink by its target (type, size, mtime).
			// Hide it when the target escapes the root, resolves to a
			// dotfile while those are blocked, or does not exist: following
			// the link would 403 or 404 anyway.
			target, err := security.PathSafe(urlPath+name, h.absRoot, blockDotfiles)
			if err != nil {
				continue
			}
			if info, err = os.Stat(target); err != nil {
				continue
			}
		}
		// Only directories and regular files are listed: a FIFO, socket or
		// device (directly or as a symlink's target) is never served, so
		// its link could only be refused.
		if !info.IsDir() && !info.Mode().IsRegular() {
			continue
		}
		if info.IsDir() {
			dirs = append(dirs, dirEntry{
				Name:    name,
				HRef:    escapeURLPath(urlPath + name + "/"),
				IsDir:   true,
				ModTime: info.ModTime().UTC().Format(modTimeFormat),
			})
		} else {
			files = append(files, dirEntry{
				Name:    name,
				HRef:    escapeURLPath(urlPath + name),
				IsDir:   false,
				Size:    info.Size(),
				ModTime: info.ModTime().UTC().Format(modTimeFormat),
			})
		}
	}

	// Sort each group alphabetically, case-insensitive.
	sort.Slice(dirs, func(i, j int) bool {
		return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name)
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})

	// Prepend ".." parent link except at root.
	allEntries := make([]dirEntry, 0, len(dirs)+len(files)+1)
	if urlPath != "/" {
		allEntries = append(allEntries, dirEntry{
			Name:  "..",
			HRef:  escapeURLPath(parentURL(urlPath)),
			IsDir: true,
		})
	}
	allEntries = append(allEntries, dirs...)
	allEntries = append(allEntries, files...)

	data := dirListData{
		URLPath:     urlPath,
		Breadcrumbs: buildBreadcrumbs(urlPath),
		Entries:     allEntries,
	}

	ctx.Response.Header.Set("Content-Type", "text/html; charset=utf-8")
	allowListingStyle(&ctx.Response.Header)
	ctx.SetStatusCode(fasthttp.StatusOK)
	// HEAD renders the body too: fasthttp never sends it, but the compress
	// middleware and Content-Length must see what GET would send, or HEAD
	// reports identity with length 0 while GET is gzipped.
	// Render template to a buffer then write to ctx.
	// SEC-010: Handle template execution errors instead of silently discarding.
	var buf bytes.Buffer
	if err := dirListTemplate.Execute(&buf, data); err != nil {
		log.Printf("handler: directory listing template error for %q: %v", urlPath, err)
		ctx.SetStatusCode(fasthttp.StatusInternalServerError)
		ctx.SetBodyString("Internal Server Error: failed to render directory listing")
		return
	}
	ctx.SetBody(buf.Bytes())
}

// buildBreadcrumbs returns breadcrumb elements for the given urlPath.
// Example: "/a/b/" → [{Label:"~", HRef:"/"}, {Label:"a", HRef:"/a/"}, {Label:"b", HRef:""}]
func buildBreadcrumbs(urlPath string) []breadcrumb {
	parts := strings.Split(strings.Trim(urlPath, "/"), "/")
	crumbs := make([]breadcrumb, 0, len(parts)+1)

	// Root always links to "/".
	crumbs = append(crumbs, breadcrumb{Label: "~", HRef: "/"})

	accumulated := "/"
	for i, p := range parts {
		if p == "" {
			continue
		}
		accumulated += p + "/"
		if i == len(parts)-1 {
			// Current directory — no hyperlink.
			crumbs = append(crumbs, breadcrumb{Label: p, HRef: ""})
		} else {
			crumbs = append(crumbs, breadcrumb{Label: p, HRef: escapeURLPath(accumulated)})
		}
	}
	return crumbs
}

// allowListingStyle rewrites the Content-Security-Policy that the security
// middleware has already set so that it also allows the listing's inline
// <style> (HTTP-F7). Adding a second CSP header would not help: every policy
// is enforced, so another header can only tighten. A response without CSP is
// left without one.
func allowListingStyle(h *fasthttp.ResponseHeader) {
	values := h.PeekAll("Content-Security-Policy")
	if len(values) == 0 {
		return
	}
	rewritten := make([]string, len(values))
	for i, v := range values {
		rewritten[i] = listingCSP(string(v))
	}
	h.Del("Content-Security-Policy")
	for _, v := range rewritten {
		h.Add("Content-Security-Policy", v)
	}
}

// listingCSP returns policy with dirListStyleSource allowed for <style>
// elements and every other restriction kept. Each comma-separated policy is
// rewritten on its own:
//   - the first style-src and style-src-elem directives get the hash;
//   - without a style-src, one is appended with the default-src sources
//     plus the hash, so styles stay exactly as restricted as before;
//   - 'none' is dropped from a source list that gains the hash;
//   - a policy with neither default-src nor any style directive already
//     allows inline styles and is returned unchanged, as is an empty policy.
func listingCSP(policy string) string {
	if strings.TrimSpace(policy) == "" {
		return policy
	}
	parts := strings.Split(policy, ",")
	for i, p := range parts {
		if r, changed := listingCSPPolicy(p); changed {
			if i > 0 {
				r = " " + r
			}
			parts[i] = r
		}
	}
	return strings.Join(parts, ",")
}

// listingCSPPolicy rewrites one serialized policy (no commas) for listingCSP.
func listingCSPPolicy(policy string) (string, bool) {
	var directives []string
	for _, d := range strings.Split(policy, ";") {
		if d = strings.TrimSpace(d); d != "" {
			directives = append(directives, d)
		}
	}
	var defaultSrc []string
	hasDefault, hasStyleSrc, changed := false, false, false
	seen := make(map[string]bool, len(directives))
	for i, d := range directives {
		fields := strings.Fields(d)
		directives[i] = strings.Join(fields, " ")
		name := strings.ToLower(fields[0])
		if seen[name] {
			continue // browsers use only the first occurrence of a directive
		}
		seen[name] = true
		switch name {
		case "default-src":
			hasDefault, defaultSrc = true, fields[1:]
		case "style-src", "style-src-elem":
			if name == "style-src" {
				hasStyleSrc = true
			}
			directives[i] = withStyleHash(fields[0], fields[1:])
			changed = true
		}
	}
	if !hasStyleSrc && hasDefault {
		directives = append(directives, withStyleHash("style-src", defaultSrc))
		changed = true
	}
	if !changed {
		return policy, false
	}
	return strings.Join(directives, "; "), true
}

// withStyleHash serializes a directive with dirListStyleSource appended to
// sources, dropping 'none' (which is only valid on its own).
func withStyleHash(name string, sources []string) string {
	out := make([]string, 0, len(sources)+2)
	out = append(out, name)
	for _, s := range sources {
		if !strings.EqualFold(s, "'none'") {
			out = append(out, s)
		}
	}
	out = append(out, dirListStyleSource)
	return strings.Join(out, " ")
}

// escapeURLPath percent-encodes a decoded URL path for use as an href, so
// that names containing "#", "?", "%" or spaces link to themselves instead of
// starting a fragment or a query (FS-5). "/" separators are kept.
// html/template still applies its own attribute escaping on top.
func escapeURLPath(p string) string {
	return (&url.URL{Path: p}).EscapedPath()
}

// parentURL returns the parent URL for a directory path.
// "/a/b/" → "/a/"   "/a/" → "/"
func parentURL(urlPath string) string {
	trimmed := strings.TrimSuffix(urlPath, "/")
	if trimmed == "" {
		return "/"
	}
	idx := strings.LastIndex(trimmed, "/")
	if idx <= 0 {
		return "/"
	}
	return trimmed[:idx+1]
}

// formatSize returns a human-readable file size string used by the template.
func formatSize(b int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case b >= gb:
		return fmt.Sprintf("%.1f GB", float64(b)/gb)
	case b >= mb:
		return fmt.Sprintf("%.1f MB", float64(b)/mb)
	case b >= kb:
		return fmt.Sprintf("%.1f KB", float64(b)/kb)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// readDirInRoot reads the entries of absDir, a directory path validated by
// this request, sorted by name. The directory is opened with
// cache.OpenSameFile after an Lstat that must find a directory, so a path
// replaced since validation (by a symlink out of the root or to another
// directory) is refused rather than listed.
func (h *FileHandler) readDirInRoot(absDir string) ([]os.DirEntry, error) {
	pre, err := os.Lstat(absDir)
	if err != nil {
		return nil, err
	}
	if !pre.IsDir() {
		return nil, cache.ErrFileChanged
	}
	d, _, err := cache.OpenSameFile(h.realRoot, absDir, pre)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	return entries, nil
}
