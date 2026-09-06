package web

import "time"

// SetNow overrides the server's clock. It exists only for tests: package
// web_test cannot reach the unexported now field directly, and session
// expiry is otherwise untestable without waiting 30 real days.
func (s *Server) SetNow(now func() time.Time) {
	s.now = now
}
