package web

import "time"

// SetNow overrides the server's clock. It exists only for tests: package
// web_test cannot reach the unexported now field directly, and session
// expiry is otherwise untestable without waiting 30 real days.
func (s *Server) SetNow(now func() time.Time) {
	s.now = now
}

// Patterns lists every route pattern registered on the server's mux, in
// registration order. http.ServeMux does not expose its own table, so this is
// what lets routes_test.go check the whole table against the authentication
// exemption list rather than spot-checking a route at a time.
func (s *Server) Patterns() []string {
	return append([]string(nil), s.patterns...)
}

// IsPublicPath exposes the exemption rule requireAuth applies, so a test can
// state the ruling for every registered route directly instead of inferring
// it from a response that two different code paths could have produced.
func IsPublicPath(path string) bool { return isPublicPath(path) }

// SetBcryptCost lowers the password hashing work factor for a test.
//
// It exists because the real cost is a second per hash-and-compare under
// -race, and this package signs an operator in a few hundred times — which
// was most of a ten-minute CI timeout, and is slow feedback for everybody
// even when it passes. Nothing outside a test calls it; a Server built
// anywhere else hashes at bcrypt.DefaultCost.
func (s *Server) SetBcryptCost(cost int) {
	s.bcryptCost = cost
}
