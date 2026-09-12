package web

import "net/http"

// settingsLink is one row on the settings index.
//
// Blurb is not decoration. An operator who knows what "Джерела" means does
// not need this page; the one who does not is exactly who opens it.
type settingsLink struct {
	Href  string
	Title string
	Blurb string
}

type settingsView struct {
	Links []settingsLink
}

// settingsLinks is the index, in the order an operator setting a device up
// for the first time works through it: where the alerts come from, what the
// device does about them, what it says, out of which output, what else it
// tells, and the clock all of it is timed by.
//
// Мережа is deliberately absent. It is the screen someone opens over the
// device's own access point when nothing else about it is reachable, and one
// level deeper is one tap more at the worst possible moment.
func settingsLinks() []settingsLink {
	return []settingsLink{
		{"/alerts", "Джерела", "Звідки пристрій дізнається про тривоги: область або громада і черга джерел."},
		{"/events", "Події", "Що і коли пристрій озвучує, і тихі години."},
		{"/sounds", "Звуки", "Бібліотека звуків пристрою."},
		// "Аудіовихід" rather than "Звук": that reads one root away from
		// "Звуки" (the sound library) directly above it, and someone
		// scanning this list under pressure — the whole reason that screen
		// exists is a fresh device that cannot make a sound at all — needs
		// to tell them apart at a glance, not by reading past the first
		// four letters.
		{"/audio", "Аудіовихід", "Куди подається звук і наскільки гучно."},
		{"/webhooks", "Вебхуки", "Запити до іншого обладнання під час оповіщення."},
		{"/time", "Час", "Годинник, часовий пояс і синхронізація."},
	}
}

// handleSettings renders the index. It reads nothing and writes nothing: the
// screens it points at own their own state, and a page that cannot fail is
// one fewer way into the dashboard that can.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	s.render(w, http.StatusOK, "settings.html", settingsView{Links: settingsLinks()})
}
