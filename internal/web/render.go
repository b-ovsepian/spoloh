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
//
// The helpers are handed in rather than built here because two of them read
// live state off one *Server — see Server.funcs.
func parseTemplates(fns template.FuncMap) (pageSet, error) {
	layout, err := template.New("layout.html").Funcs(fns).ParseFS(templateFS, "templates/layout.html")
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

// funcs are the helpers every template is parsed with.
//
// Two of them close over this server. The layout needs the alert state and
// the running version, and neither is reachable from the dot it is executed
// with: render hands each page its own view struct and there is no shared
// base view. The alternative was an envelope struct wrapping every one of
// those structs, which renames every {{.Field}} in every template for the
// sake of two values the layout alone reads. parseTemplates runs once per
// server, from New, so a closure over that server is exactly as long-lived
// as the template set it is bound into.
func (s *Server) funcs() template.FuncMap {
	return template.FuncMap{
		// hasWeekday backs events.html's weekday checkboxes: a template has
		// no way to ask "is this int in that slice" on its own.
		"hasWeekday": hasWeekday,
		"alertState": s.alertStateWord,
		"version":    s.versionText,
	}
}

// alertStateWord is what the browser tab leads with, so an operator with the
// dashboard open behind other windows can read the state off the tab strip
// without switching to it.
//
// It asks the engine directly rather than going through Server.status, which
// also resolves the region and walks the event list for the next scheduled
// announcement through a fresh scheduler.Runner. This runs on every render of
// every page and needs one boolean; the rest of that work would be built and
// thrown away seven pages out of eight.
//
// No engine — before the daemon wires the dashboard up, and in this package's
// own tests — reads as calm, the same default the zero-value Status gives the
// banner. A tab that said «Тривога» because a dependency was missing would be
// the one kind of wrong this screen must never be.
func (s *Server) alertStateWord() string {
	if s.deps.Engine == nil || !s.deps.Engine.Status().Alert {
		return "Спокійно"
	}
	return "Тривога"
}

// versionText is the build identity for the footer.
//
// Two strings mean "nobody stamped this": the empty Deps default, and the
// "dev" that cmd/spoloh's main.version carries when it is built without the
// Makefile's -ldflags. Both say so in words — a footer reading «Версія:» with
// nothing after it looks like a rendering fault rather than an answer, and
// the operator reading it aloud over the phone needs to know the difference
// between "no version" and "I cannot see one".
//
// Anything else is printed as it stands, including the bare commit hash a
// local `make build` produces: `git describe --tags --always --dirty` falls
// back to the hash when the checkout has no tag, and that hash is a real
// identity somebody can look up, not a placeholder.
func (s *Server) versionText() string {
	if s.deps.Version == "" || s.deps.Version == "dev" {
		return "збірка для розробки"
	}
	return s.deps.Version
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
