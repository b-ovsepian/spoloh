package web

import (
	"bytes"
	"html/template"
	"io/fs"
	"net/http"
	"path"
)

// pageSet holds one *template.Template per page, each rooted at its own
// clone of the shared layout.
//
// html/template.ParseFS would otherwise put every page file into one flat
// template set. Since every page defines "content" (and most define
// "title"), that would make the last page parsed win those block names for
// every route — which is exactly how an earlier version of this package
// served setup.html's content to /login and vice versa.
type pageSet map[string]*template.Template

// parseTemplates compiles every page against the shared layout at startup, so
// a broken template fails the daemon's start rather than a request at 3 a.m.
func parseTemplates() (pageSet, error) {
	layout, err := template.New("layout.html").Funcs(funcs()).ParseFS(templateFS, "templates/layout.html")
	if err != nil {
		return nil, err
	}

	matches, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, err
	}

	pages := make(pageSet, len(matches))
	for _, match := range matches {
		name := path.Base(match)
		if name == "layout.html" {
			continue
		}
		page, err := layout.Clone()
		if err != nil {
			return nil, err
		}
		if page, err = page.ParseFS(templateFS, match); err != nil {
			return nil, err
		}
		pages[name] = page
	}
	return pages, nil
}

func funcs() template.FuncMap {
	return template.FuncMap{
		// hasWeekday backs events.html's weekday checkboxes: a template has
		// no way to ask "is this int in that slice" on its own.
		"hasWeekday": hasWeekday,
	}
}

// render writes a page, buffering first so a template that fails half-way
// produces a clean 500 instead of a truncated page with a 200 already sent.
// The Content-Type header is set before WriteHeader: net/http silently drops
// headers set afterward, which httptest does not reproduce in tests.
func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	page, ok := s.pages[name]
	if !ok {
		s.deps.Logger.Error("no such template", "template", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := page.ExecuteTemplate(&buf, "layout", data); err != nil {
		s.deps.Logger.Error("template failed", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = buf.WriteTo(w)
}
