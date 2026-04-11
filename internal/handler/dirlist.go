package handler

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"sort"
	"strings"

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
  <style>
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
  </style>
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
	entries, err := os.ReadDir(absDir)
	if err != nil {
		if os.IsPermission(err) {
			ctx.Error("Forbidden", fasthttp.StatusForbidden)
			return
		}
		ctx.Error("Internal Server Error", fasthttp.StatusInternalServerError)
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
		href := urlPath + name
		if e.IsDir() {
			href += "/"
			dirs = append(dirs, dirEntry{
				Name:    name,
				HRef:    href,
				IsDir:   true,
				ModTime: info.ModTime().UTC().Format(modTimeFormat),
			})
		} else {
			files = append(files, dirEntry{
				Name:    name,
				HRef:    href,
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
			HRef:  parentURL(urlPath),
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
	ctx.SetStatusCode(fasthttp.StatusOK)
	if ctx.IsHead() {
		return
	}
	// Render template to a buffer then write to ctx.
	// SEC-010: Handle template execution errors instead of silently discarding.
	var buf bytes.Buffer
	if err := dirListTemplate.Execute(&buf, data); err != nil {
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
			crumbs = append(crumbs, breadcrumb{Label: p, HRef: accumulated})
		}
	}
	return crumbs
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
