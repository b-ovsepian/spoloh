package web

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"

	"github.com/b-ovsepian/spoloh/internal/config"
)

type monitoringView struct {
	Message string
	Error   string

	Enabled bool
	// Rev names the token the page was rendered with; see tokenRev.
	Rev string
	// Token is set only on the response that generated it: it is never
	// stored, so it can never be shown again.
	Token string
	// URL is /metrics as this browser reached the device, for the examples.
	URL string
}

func (s *Server) handleMonitoringPage(w http.ResponseWriter, r *http.Request) {
	s.renderMonitoringPage(w, r, http.StatusOK, nil)
}

// handleMonitoringSave generates a token or switches monitoring off. Each
// needs its own button: an empty form, or one with an action this does not
// know, changes nothing.
func (s *Server) handleMonitoringSave(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		http.Error(w, "cross-origin request rejected", http.StatusForbidden)
		return
	}
	switch r.FormValue("action") {
	case "generate":
		// The form carries the revision of the token it was shown with.
		// Sent again — F5 on the page that showed a new token, back and
		// forward — it names a token that is no longer current and changes
		// nothing: otherwise the token just copied into the monitoring
		// system would be replaced unseen, and monitoring would go dark.
		if r.FormValue("rev") != tokenRev(s.deps.Config.Get().Monitoring.TokenHash) {
			s.renderMonitoringPage(w, r, http.StatusOK, func(v *monitoringView) {
				v.Message = "Цю форму вже надіслано: токен відтоді змінено, тож новий не створено."
			})
			return
		}
		token, hash, err := newMonitoringToken()
		if err == nil {
			err = s.deps.Config.Update(func(c *config.Config) error {
				c.Monitoring.TokenHash = hash
				return nil
			})
		}
		if err != nil {
			s.deps.Logger.Error("monitoring: could not make a token", "err", err)
			s.renderMonitoringPage(w, r, http.StatusInternalServerError, func(v *monitoringView) {
				v.Error = "Не вдалося створити токен."
			})
			return
		}
		s.renderMonitoringPage(w, r, http.StatusOK, func(v *monitoringView) {
			v.Token = token
			v.Message = "Токен створено. Скопіюйте його зараз — пристрій його не зберігає і більше не покаже."
		})
	case "disable":
		if err := s.deps.Config.Update(func(c *config.Config) error {
			c.Monitoring = config.Monitoring{}
			return nil
		}); err != nil {
			s.deps.Logger.Error("monitoring: could not switch off", "err", err)
			s.renderMonitoringPage(w, r, http.StatusInternalServerError, func(v *monitoringView) {
				v.Error = "Не вдалося вимкнути моніторинг."
			})
			return
		}
		s.renderMonitoringPage(w, r, http.StatusOK, func(v *monitoringView) { v.Message = "Моніторинг вимкнено." })
	default:
		s.renderMonitoringPage(w, r, http.StatusOK, nil)
	}
}

func (s *Server) renderMonitoringPage(w http.ResponseWriter, r *http.Request, status int, mutate func(*monitoringView)) {
	hash := s.deps.Config.Get().Monitoring.TokenHash
	view := monitoringView{
		Enabled: hash != "",
		Rev:     tokenRev(hash),
		URL:     "http://" + r.Host + metricsPath,
	}
	if mutate != nil {
		mutate(&view)
	}
	// One of these responses shows a token, and a browser must not keep it.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, status, "monitoring.html", view)
}

// tokenRev names a stored token without revealing anything of it: a hash of
// its hash, cut short, or "none" when there is none. It only has to tell one
// token from the next.
func tokenRev(hash string) string {
	if hash == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte("rev:" + hash))
	return hex.EncodeToString(sum[:6])
}

// newMonitoringToken makes a token and the hash that is kept of it: 32
// random bytes, which no amount of guessing at a LAN device reaches.
func newMonitoringToken() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	sum := sha256.Sum256([]byte(token))
	return token, hex.EncodeToString(sum[:]), nil
}
