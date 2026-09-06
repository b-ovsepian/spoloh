package web

import (
	"bytes"
	"html/template"
	"net/http"
)

// parseTemplates compiles every page against the shared layout at startup, so a
// broken template fails the daemon's start rather than a request at 3 a.m.
func parseTemplates() (*template.Template, error) {
	return template.New("").Funcs(funcs()).ParseFS(templateFS, "templates/*.html")
}

func funcs() template.FuncMap {
	return template.FuncMap{}
}

// render writes a page, buffering first so a template that fails half-way
// produces a clean 500 instead of a truncated page with a 200 already sent.
func (s *Server) render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := s.tmpl.ExecuteTemplate(&buf, name, data); err != nil {
		s.deps.Logger.Error("template failed", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = buf.WriteTo(w)
}
