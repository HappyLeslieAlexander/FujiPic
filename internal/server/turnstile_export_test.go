package server

import "net/http"

// SetTurnstileTransport is compiled only into the test binary.
func (s *Server) SetTurnstileTransport(transport http.RoundTripper) {
	s.turnstileClient.Transport = transport
}
