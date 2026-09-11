package web_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/b-ovsepian/spoloh/internal/configstore"
	"github.com/b-ovsepian/spoloh/internal/library"
	"github.com/b-ovsepian/spoloh/internal/web"
)

// makeWAV builds a minimal, structurally valid WAV file with the given
// format, mirroring internal/library's own test helper of the same name:
// that one is unexported and lives in a different package, so this screen's
// tests need their own copy to build upload bodies that library.Add accepts
// without ever needing ffmpeg (forbidden in this suite — see the package
// doc on TestNoPageRendersAnAuthSecret and the task brief's global
// constraints).
func makeWAV(formatTag, channels uint16, sampleRate uint32, bitsPerSample uint16, data []byte) []byte {
	blockAlign := channels * (bitsPerSample / 8)
	byteRate := sampleRate * uint32(blockAlign)

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(data)))
	buf.WriteString("WAVE")

	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, formatTag)
	binary.Write(&buf, binary.LittleEndian, channels)
	binary.Write(&buf, binary.LittleEndian, sampleRate)
	binary.Write(&buf, binary.LittleEndian, byteRate)
	binary.Write(&buf, binary.LittleEndian, blockAlign)
	binary.Write(&buf, binary.LittleEndian, bitsPerSample)

	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)

	return buf.Bytes()
}

// conformingWAV is PCM 16-bit 44100 Hz stereo: the only format library.Add
// accepts without running it through a converter, so an upload built from
// this never touches ffmpeg.
func conformingWAV() []byte {
	return makeWAV(1, 2, 44100, 16, []byte("payload"))
}

// uploadRequest builds a multipart POST to /sounds/upload, as the task brief
// specifies: a hand-written multipart body is too easy to get subtly wrong,
// where mime/multipart cannot be.
func uploadRequest(t *testing.T, filename string, body []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	part, err := w.CreateFormFile("sound", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sounds/upload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", "http://"+req.Host)
	return req
}

func TestSoundsPageRequiresSession(t *testing.T) {
	srv, _ := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sounds", nil))

	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("GET /sounds with no session = %d %q, want 303 to /login", rec.Code, rec.Header().Get("Location"))
	}
}

// library.Sound carries no duration, so this only asserts on the two fields
// it does carry: Size and ModTime, rendered as the page's own text rather
// than a Go zero value or nothing at all.
func TestSoundsPageListsEverySoundWithSizeAndModTime(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "custom-announcement.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()

	if !strings.Contains(body, "custom-announcement.wav") {
		t.Error("page does not list the sound by name")
	}
	// newServerWithLibrary writes "RIFF____WAVE", 12 bytes.
	if !strings.Contains(body, "12 Б") {
		t.Errorf("page does not show the sound's size; body:\n%s", body)
	}
	if !regexp.MustCompile(`\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}`).MatchString(body) {
		t.Error("page does not show a modification timestamp")
	}
}

func TestSoundsUploadRejectsCrossOrigin(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := uploadRequest(t, "new.wav", conformingWAV())
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if strings.Contains(rec2.Body.String(), "new.wav") {
		t.Error("the cross-origin upload was stored anyway")
	}
}

func TestSoundsUploadStoresFileAndItAppearsInList(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := uploadRequest(t, "announcement.wav", conformingWAV())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "announcement.wav") {
		t.Error("the re-rendered page does not list the uploaded sound")
	}

	// Confirm it is also there on a fresh GET, not just echoed by the POST's
	// own re-render.
	req2 := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), "announcement.wav") {
		t.Error("the sound does not appear in the library after upload")
	}
}

// A file that is not audio at all must re-render with a generic Ukrainian
// message, not library.Add's own error text: for the "everything else"
// case (anything that is not one of the two named sentinels), that text can
// be developer English or, on a failed conversion, a full ffmpeg stderr
// transcript — meaningless to someone who just tried to upload a recording
// of a school bell. Nothing must be added either way.
func TestSoundsUploadWithBadAudioGivesAGenericUkrainianMessage(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	// A .wav file that is not a real WAV at all: parseWAV rejects it before
	// any converter would ever run, with an error that is neither
	// ErrConversionUnavailable nor ErrBadName.
	req := uploadRequest(t, "bad.wav", []byte("this is not a wav file"))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "аудіо") {
		t.Errorf("the rejection is not the generic Ukrainian message; body:\n%s", body)
	}
	if strings.Contains(body, "RIFF/WAVE") {
		t.Errorf("the rejection leaked the library's own developer-English error; body:\n%s", body)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if strings.Contains(rec2.Body.String(), "bad.wav") {
		t.Error("a rejected upload was added to the library anyway")
	}
}

// An unsafe filename (library.ErrBadName) is specific and actionable enough
// to translate on its own, and sanitise() is checked before Add ever reads
// the body or asks a converter whether it is available — so this needs no
// real audio content and never risks touching ffmpeg.
func TestSoundsUploadWithAnUnsafeFilenameNamesTheProblem(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := uploadRequest(t, "bad name.mp3", []byte("irrelevant"))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "назв") {
		t.Errorf("the rejection does not mention the filename; body:\n%s", rec.Body.String())
	}
}

// fakeUnavailableConverter simulates a device with no ffmpeg installed, so
// the ErrConversionUnavailable mapping can be exercised without the real
// ffmpeg binary this suite must never run.
type fakeUnavailableConverter struct{}

func (fakeUnavailableConverter) Available() bool { return false }
func (fakeUnavailableConverter) ToWAV(context.Context, string, string) error {
	return errors.New("must not be called: Available reported false")
}

// ErrConversionUnavailable is the one case worth naming the required format
// for: an operator with no ffmpeg on the device can still fix this, by
// converting the file elsewhere, if the message says what to convert it to.
func TestSoundsUploadWhenConversionIsUnavailableNamesTheRequiredFormat(t *testing.T) {
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	lib := library.New(t.TempDir(), fakeUnavailableConverter{})
	srv, err := web.New(web.Deps{Config: store, Library: lib})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := uploadRequest(t, "clip.mp3", []byte("not really mp3 audio"))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	// "WAV" alone is not specific enough: the generic fallback message
	// ("Спробуйте WAV або MP3.") also contains it, so a test asserting only
	// that would still pass if this branch silently degraded into the
	// generic one. PCM/44100 appear only in the curated
	// ErrConversionUnavailable sentence, which is the actual point of this
	// test — see uploadRejectionMessage.
	if !strings.Contains(body, "PCM") || !strings.Contains(body, "44100") {
		t.Errorf("the rejection does not name the required format precisely; body:\n%s", body)
	}
	if strings.Contains(body, "ErrConversionUnavailable") || strings.Contains(body, "ffmpeg is not installed") {
		t.Errorf("the rejection leaked the Go error rather than a Ukrainian sentence; body:\n%s", body)
	}
}

func TestSoundsUploadWithNoFileSaysSo(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/sounds/upload", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}

	req2 := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	// Only the one sound newServerWithLibrary seeded must be present. Each
	// row renders twice (the mobile list and the table, per sounds.html's
	// responsive split), so this counts the mobile <li> wrapper, which
	// appears exactly once per sound.
	if got := strings.Count(rec2.Body.String(), `<li class="rounded-md border border-border p-3">`); got != 1 {
		t.Errorf("library has %d rows after an empty upload, want 1 (unchanged); body:\n%s", got, rec2.Body.String())
	}
}

// The board this runs on has 512 MB of RAM and another job to do; an upload
// past the ceiling must be refused in Ukrainian, not silently truncated or
// left to OOM the process.
func TestSoundsUploadRejectsAFileOverTheLimit(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	oversized := make([]byte, 17<<20) // over the 16 MiB ceiling
	req := uploadRequest(t, "huge.wav", oversized)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	// "16" alone is not specific enough: sounds.html also renders each
	// sound's modification time as HH:MM:SS, so during the 16th hour of any
	// day a seeded sound's own timestamp would contain "16" and this would
	// pass regardless of whether the limit message rendered at all.
	if !strings.Contains(rec.Body.String(), "Файл завеликий") {
		t.Errorf("the rejection does not mention the limit; body:\n%s", rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if strings.Contains(rec2.Body.String(), "huge.wav") {
		t.Error("an over-the-limit upload was stored anyway")
	}
}

// Deleting a sound an event uses leaves the event pointing at a file that is
// gone. config.Validate cannot catch it — the library is on disk, not in the
// config — so the only thing standing between an operator and a silent alert
// is this check.
func TestSoundsRefusesToDeleteASoundAnEventUses(t *testing.T) {
	srv, store := newServerWithStore(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	inUse := store.Get().Events[0].Sound // whatever alert-start plays

	form := url.Values{"name": {inUse}}
	req := httptest.NewRequest(http.MethodPost, "/sounds/delete", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://"+req.Host)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, inUse) {
		t.Errorf("the refusal does not name the sound: %s", body)
	}
	// The message must name the events holding it, or the operator has no way
	// to find out what to change first.
	if !strings.Contains(body, "alert-start") {
		t.Errorf("the refusal does not name the event using it: %s", body)
	}
}

func TestSoundsDeleteRemovesAnUnreferencedSound(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav", "all-clear.wav", "metronome.wav", "spare.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := eventsPostForm(t, srv, cookie, "/sounds/delete", url.Values{"name": {"spare.wav"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	if strings.Contains(rec2.Body.String(), "spare.wav") {
		t.Error("the unreferenced sound is still listed after deletion")
	}
}

func TestSoundsPageRendersWithNilLibrary(t *testing.T) {
	srv, _ := newServerWithStore(t) // Deps.Library is nil
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even with no library", rec.Code)
	}
}
