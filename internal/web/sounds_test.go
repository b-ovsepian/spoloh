package web_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

// newServerWithLibraryDir is newServerWithLibrary's counterpart for fixtures
// too large to reasonably POST through /sounds/upload's own maxUploadBytes
// ceiling: it hands the test the library directory itself, to write into
// directly, the same way an existing file the operator did not upload
// through this form is already sitting there when the daemon starts.
func newServerWithLibraryDir(t *testing.T) (*web.Server, string) {
	t.Helper()
	store, err := configstore.Open(filepath.Join(t.TempDir(), "config.json"))
	if err != nil {
		t.Fatalf("configstore.Open: %v", err)
	}
	dir := t.TempDir()
	lib := library.New(dir, library.FFmpeg{})
	srv, err := web.New(web.Deps{Config: store, Library: lib})
	if err != nil {
		t.Fatalf("web.New: %v", err)
	}
	return srv, dir
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
// of a doorbell. Nothing must be added either way.
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

// The upload form's progress bar is Alpine plus XMLHttpRequest, and Go can
// run neither. Nothing below proves that a bar animates, that
// xhr.upload.onprogress fires, that the «Обробка…» state appears when the
// bytes are gone, or that a browser with JavaScript off still posts the form
// — those are the browser's half of this feature and they were checked by
// hand. What Go can see is the other half: that the server offers a JSON
// representation for the script to read, that it says the same sentences the
// page would have said, and that an ordinary form post is completely
// unaffected by any of it. That last one is what matters most here: this
// device is safety equipment and its upload form must keep working when the
// script does not.

// jsonUploadRequest is the request the page's XHR makes: the same URL and the
// same multipart body as the plain form, asking only for a different
// representation of the answer.
func jsonUploadRequest(t *testing.T, filename string, body []byte) *http.Request {
	t.Helper()
	req := uploadRequest(t, filename, body)
	req.Header.Set("Accept", "application/json")
	return req
}

func decodeUploadResult(t *testing.T, rec *httptest.ResponseRecorder) struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
} {
	t.Helper()
	var result struct {
		OK      bool   `json:"ok"`
		Message string `json:"message"`
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json; body:\n%s", ct, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("response is not the JSON result: %v; body:\n%s", err, rec.Body.String())
	}
	return result
}

func TestSoundsUploadAnswersJSONWhenAsked(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := jsonUploadRequest(t, "announcement.wav", conformingWAV())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body:\n%s", rec.Code, rec.Body.String())
	}
	// A whole rendered page instead of the small result would be many
	// kilobytes of HTML the script cannot read, on a link the operator is
	// standing next to the device waiting on.
	if strings.Contains(rec.Body.String(), "<!doctype") {
		t.Errorf("the JSON branch rendered the page instead; body:\n%s", rec.Body.String())
	}
	result := decodeUploadResult(t, rec)
	if !result.OK {
		t.Errorf("ok = false for a successful upload; body:\n%s", rec.Body.String())
	}
	if !strings.Contains(result.Message, "announcement.wav") {
		t.Errorf("the confirmation does not name the sound: %q", result.Message)
	}

	// The script reloads the page on success, so the sound has to actually be
	// in the library and not merely reported as added.
	req2 := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if !strings.Contains(rec2.Body.String(), "announcement.wav") {
		t.Error("the sound does not appear in the library after a JSON upload")
	}
}

// The whole point of the JSON branch is that the script has a sentence to put
// on the screen. It must be the same curated Ukrainian one the page would have
// shown — not library.Add's own error, which is developer English at best and
// an ffmpeg stderr transcript at worst.
func TestSoundsUploadJSONCarriesTheServersOwnRejectionSentence(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := jsonUploadRequest(t, "bad.wav", []byte("this is not a wav file"))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	result := decodeUploadResult(t, rec)
	if result.OK {
		t.Error("ok = true for a rejected upload")
	}
	if !strings.Contains(result.Message, "аудіо") {
		t.Errorf("the rejection is not the generic Ukrainian message: %q", result.Message)
	}
	if strings.Contains(result.Message, "RIFF/WAVE") {
		t.Errorf("the rejection leaked the library's own developer-English error: %q", result.Message)
	}
}

// An ordinary browser form post — which is what the device gets whenever the
// script does not run — must be completely unaffected: the whole page back,
// with the rejection in it. A browser's Accept header ends in "*/*", and
// matching that would have quietly turned every no-JavaScript submit into a
// JSON document the operator would be left staring at.
func TestSoundsUploadFromAPlainFormStillGetsTheWholePage(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := uploadRequest(t, "bad.wav", []byte("this is not a wav file"))
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body:\n%s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<!doctype html>") {
		t.Errorf("a plain form post did not get the page back; body:\n%s", body)
	}
	if !strings.Contains(body, "аудіо") {
		t.Errorf("the re-rendered page does not carry the rejection; body:\n%s", body)
	}
}

// The progress bar is an enhancement layered onto a form that works without
// it. If this ever becomes a button that only a script can submit, the device
// loses the ability to be given a siren from a browser with JavaScript off —
// so the plain form is pinned here rather than left to a reviewer's eye.
func TestSoundsPageKeepsAFormThatPostsWithoutJavaScript(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `<form method="post" action="/sounds/upload" enctype="multipart/form-data"`) {
		t.Errorf("the upload form is no longer a plain multipart POST; body:\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `type="submit"`) {
		t.Errorf("the upload form has no submit button; body:\n%s", rec.Body.String())
	}
}

// --- Duration --------------------------------------------------------------
//
// «на Бібліотека звуків показувати тривалість звуку» — the operator's
// request this answers. Size in megabytes does not say how long a sound
// plays, and on this device that matters: an event repeats a sound a
// configured number of times and quiet hours are measured in minutes.

// wavOfDuration builds a conforming (PCM 16-bit 44100 Hz stereo) WAV of
// exactly the given length, so a test can assert on the page's own
// formatted text ("0:12") instead of an arithmetic result it has to trust.
func wavOfDuration(seconds int) []byte {
	const byteRate = 44100 * 2 * 2 // sampleRate * channels * bytesPerSample
	return makeWAV(1, 2, 44100, 16, make([]byte, byteRate*seconds))
}

// wavWithZeroByteRate conforms on every field library.Add checks (format
// tag, channel count, sample rate, bit depth) except the fmt chunk's own
// ByteRate field, which no real encoder writes as zero. It is the one way
// to get an unknown-duration sound into the library through the same
// upload path an operator uses, rather than by writing into the library
// directory directly the way library_test.go's equivalent fixture does.
func wavWithZeroByteRate() []byte {
	data := []byte("some audio payload, its content does not matter here")
	const blockAlign = 2 * (16 / 8) // channels * bytesPerSample

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(data)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(2)) // stereo
	binary.Write(&buf, binary.LittleEndian, uint32(44100))
	binary.Write(&buf, binary.LittleEndian, uint32(0)) // the malformed field under test
	binary.Write(&buf, binary.LittleEndian, uint16(blockAlign))
	binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(data)))
	buf.Write(data)
	return buf.Bytes()
}

// Covers a sub-minute, a minute-plus and a multi-minute value — the three
// shapes humanDuration's "%d:%02d" formatting can produce — asserted against
// both of sounds.html's two renderings of the row (the table cell and the
// mobile card's "duration · size" line). Each expected string is matched
// with a boundary on both sides so a coincidental digit run inside a
// modification timestamp rendered elsewhere on the same page (HH:MM:SS)
// cannot be mistaken for it.
//
// The fixtures are written straight into the library directory rather than
// posted through /sounds/upload: 4 minutes 30 seconds of conforming PCM
// audio is about 45 MB, well past maxUploadBytes's 16 MiB ceiling on the
// upload form itself, but an ordinary file already sitting in the library
// directory — restored from a backup, copied over SSH, present before that
// ceiling existed — is exactly what List reads on every page load regardless
// of how it got there.
//
// Mutation to prove this test: format duration as a Go duration string
// (d.String(), e.g. "1m5s") instead of "%d:%02d". None of "1m5s", "4m30s" or
// "12s" match this test's expectations.
func TestSoundsPageRendersDurationForEachMagnitude(t *testing.T) {
	srv, dir := newServerWithLibraryDir(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	for _, tc := range []struct {
		filename string
		seconds  int
	}{
		{"short.wav", 12},  // sub-minute
		{"medium.wav", 65}, // minute-plus
		{"long.wav", 270},  // multi-minute
	} {
		if err := os.WriteFile(filepath.Join(dir, tc.filename), wavOfDuration(tc.seconds), 0o644); err != nil {
			t.Fatalf("WriteFile %s: %v", tc.filename, err)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, want := range []string{"0:12", "1:05", "4:30"} {
		if !strings.Contains(body, ">"+want+"<") {
			t.Errorf("the table does not show duration %q; body:\n%s", want, body)
		}
		if !strings.Contains(body, want+" ·") {
			t.Errorf("the mobile card does not show duration %q; body:\n%s", want, body)
		}
	}
}

// A duration that cannot be determined must say so, not claim the sound is
// silent. "0:00" and "I could not tell" are different facts and an operator
// reads them differently — an event pointed at a sound that actually plays
// for four minutes is not something this page may hide behind a wrong zero.
//
// Mutation to prove this test: have humanDuration format the unknown case as
// "0:00" instead of returning the unknown-duration text. This test's checks
// for the exact rendered patterns "0:00 ·" and ">0:00<" then fail, because
// that is exactly what the mutation would produce.
func TestSoundsPageRendersUnknownDurationRatherThanZero(t *testing.T) {
	srv, _ := newServerWithLibrary(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := uploadRequest(t, "bad-rate.wav", wavWithZeroByteRate())
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("uploading bad-rate.wav: status = %d, want 200 (it conforms on every field library.Add checks); body:\n%s", rec.Code, rec.Body.String())
	}

	req2 := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req2.AddCookie(cookie)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	body := rec2.Body.String()

	if !strings.Contains(body, "bad-rate.wav") {
		t.Fatalf("the uploaded sound is not listed; body:\n%s", body)
	}
	if strings.Contains(body, "0:00 ·") || strings.Contains(body, ">0:00<") {
		t.Errorf("an unknown duration rendered as 0:00; body:\n%s", body)
	}
	if !strings.Contains(body, "невідомо") {
		t.Errorf("an unknown duration does not say so; body:\n%s", body)
	}
}

// --- Preview -------------------------------------------------------------
//
// The operator's complaint this answers: «все ще не можу прослухати звуки в
// розділі Звуки». /sounds listed what the library held and offered no way to
// hear any of it, so telling the siren from the all-clear meant deleting one
// and finding out during an air raid.

// playForm posts to /play the way a row's preview button does — the sound
// and the page to come back to — through the full handler stack, so the
// session and same-origin checks are exercised rather than bypassed.
func playForm(t *testing.T, srv *web.Server, cookie *http.Cookie, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	return eventsPostForm(t, srv, cookie, "/play", form)
}

// The preview has to reach the real backend, not merely return a redirect: a
// handler that answered 303 and played nothing would satisfy every status
// assertion in this file while leaving the operator listening to silence.
// newServerWithPlayer wires a real engine and player over a recording
// backend, so this asserts on what was actually handed to playback.
func TestSoundsPreviewPlaysTheNamedSoundThroughTheEngine(t *testing.T) {
	srv, _, backend := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := playForm(t, srv, cookie, url.Values{"sound": {"siren.wav"}, "return": {"/sounds"}})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	if got := waitPlayed(t, backend); got != "siren.wav" {
		t.Fatalf("the device played %q, want siren.wav", got)
	}
}

// A preview that bounced the operator to the dashboard would make previewing
// three uploads in a row unusable: the list they are working through is on
// /sounds, and every press would take them off it.
func TestSoundsPreviewReturnsToTheSoundsPage(t *testing.T) {
	srv, _, backend := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := playForm(t, srv, cookie, url.Values{"sound": {"siren.wav"}, "return": {"/sounds"}})
	waitPlayed(t, backend)

	if loc := rec.Header().Get("Location"); loc != "/sounds?played=siren.wav" {
		t.Fatalf("redirected to %q, want /sounds?played=siren.wav", loc)
	}
}

// The one security-relevant line in this feature. A return value is taken
// from a form and reflected into a Location header, so anything that can
// name a host off this device turns the dashboard into an open redirect: a
// crafted link would carry an operator away from the device's own address
// onto a page of someone else's choosing.
func TestPlayRefusesAnOffSiteReturnPath(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	for _, off := range []string{
		"http://evil.example/x",
		"https://evil.example/x",
		"//evil.example/x",
		`/\evil.example`,
		"evil.example/x",
		"sounds",
		"javascript:alert(1)",
		"mailto:someone@evil.example",
		"http://evil.example",
	} {
		t.Run(off, func(t *testing.T) {
			// Not waiting on the backend here, unlike the tests above: the
			// recording backend blocks on Play until cancelled, so only the
			// first of these ever starts and the rest sit in the player's
			// queue. What is under test is the Location, and the play is
			// deliberately allowed to succeed so that a fallback cannot be
			// mistaken for a refusal to play at all.
			rec := playForm(t, srv, cookie, url.Values{"sound": {"siren.wav"}, "return": {off}})

			loc := rec.Header().Get("Location")
			if loc != "/?played=siren.wav" {
				t.Fatalf("return=%q redirected to %q, want the /?played=siren.wav fallback", off, loc)
			}
		})
	}
}

// A name that is not in the library is the case an operator reaches by
// pressing a preview for a sound another tab has just deleted. It must be
// refused, it must not panic, and — because they are standing on /sounds —
// the refusal has to be readable there rather than on a bare error page.
func TestSoundsPreviewOfAMissingSoundIsRefusedOnTheSoundsPage(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := playForm(t, srv, cookie, url.Values{"sound": {"../../etc/passwd"}, "return": {"/sounds"}})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303 back to /sounds; body:\n%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if loc != "/sounds?playfailed=missing" {
		t.Fatalf("redirected to %q, want /sounds?playfailed=missing", loc)
	}

	req := httptest.NewRequest(http.MethodGet, loc, nil)
	req.AddCookie(cookie)
	page := httptest.NewRecorder()
	srv.Handler().ServeHTTP(page, req)
	if !strings.Contains(page.Body.String(), "Такого звука немає в бібліотеці.") {
		t.Errorf("the sounds page does not say why the preview failed; body:\n%s", page.Body.String())
	}
}

// The flash is the whole point of the return path: without it the operator
// gets a page that looks exactly as it did before they pressed anything, and
// no way to tell a preview that started from one that was refused.
func TestSoundsPageShowsThePreviewFlash(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	for _, tc := range []struct {
		query string
		want  string
	}{
		{"/sounds?played=siren.wav", "Відтворюється: siren.wav"},
		{"/sounds?playfailed=missing", "Такого звука немає в бібліотеці."},
		{"/sounds?playfailed=unavailable", "Відтворення недоступне на цьому пристрої."},
	} {
		t.Run(tc.query, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.query, nil)
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("%s does not carry %q; body:\n%s", tc.query, tc.want, rec.Body.String())
			}
		})
	}
}

// An unrecognised code — a hand-edited link, a redirect from an older build
// still in a browser's history — must not be echoed onto the device's own
// dashboard dressed as something the device said. Same rule flashMessage
// already applies to "played".
func TestSoundsPageDoesNotEchoAnUnknownFailureCode(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/sounds?playfailed="+url.QueryEscape("Тривога! Це не тест"), nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if strings.Contains(body, "Тривога! Це не тест") {
		t.Error("an unvalidated playfailed value was echoed onto the page")
	}
	if !strings.Contains(body, "Не вдалося відтворити звук.") {
		t.Errorf("the generic fallback sentence is missing; body:\n%s", body)
	}
}

// The preview is a plain form for the same reason the upload form is: this
// device is safety equipment, and the only way to tell the siren from the
// all-clear must not depend on a script being alive.
func TestSoundsPageRendersAPreviewFormPerRowWithoutJavaScript(t *testing.T) {
	srv, _ := newServerWithLibrary(t, "siren.wav", "all-clear.wav")
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	req := httptest.NewRequest(http.MethodGet, "/sounds", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if n := strings.Count(body, `<form method="post" action="/play">`); n != 4 {
		// Two sounds, each rendered twice: the stacked card below sm and the
		// table above it.
		t.Errorf("page has %d preview forms, want 4 (2 sounds in both the card and table renderings); body:\n%s", n, body)
	}
	for _, want := range []string{
		`<input type="hidden" name="sound" value="siren.wav">`,
		`<input type="hidden" name="sound" value="all-clear.wav">`,
		`<input type="hidden" name="return" value="/sounds">`,
		"Прослухати",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the preview form is missing %q; body:\n%s", want, body)
		}
	}
	if strings.Contains(body, `action="/play"`) && !strings.Contains(body, `type="submit">Прослухати`) {
		t.Error("the preview is not a submit button on a plain form")
	}
}

// /play is the dashboard's manual playback as well, and nothing about adding
// a return field may change what the home page's own form — which names no
// return at all — has always done: 303 to /?played= on success, a plain
// error response on a sound that is not there.
func TestPlayWithoutAReturnPathIsUnchanged(t *testing.T) {
	srv, _, backend := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	rec := playForm(t, srv, cookie, url.Values{"sound": {"siren.wav"}})
	waitPlayed(t, backend)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/?played=siren.wav" {
		t.Fatalf("redirected to %q, want /?played=siren.wav", loc)
	}

	rec = playForm(t, srv, cookie, url.Values{"sound": {"nope.wav"}})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a missing sound with no return path = %d, want 400 — the home page's own error response", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Такого звука немає в бібліотеці.") {
		t.Errorf("the 400 does not name the problem; body:\n%s", rec.Body.String())
	}
}

// The same-origin check every mutating handler here uses applies to the
// preview too, and it has to keep applying: /play is reached from a new page
// now, and a reviewer could reasonably wonder whether that changed anything.
func TestSoundsPreviewRejectsCrossOrigin(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	form := url.Values{"sound": {"siren.wav"}, "return": {"/sounds"}}
	req := httptest.NewRequest(http.MethodPost, "/play", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://evil.example")
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// Stopping from /sounds must answer on /sounds. Previewing a sixty-second
// siren and then having to walk to the dashboard to silence it is how the
// absence of this was noticed; a stop that redirects away is the same
// defect wearing a button.
func TestStopReturnsToThePageThatAskedForIt(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	form := url.Values{"return": {"/sounds"}}
	req := httptest.NewRequest(http.MethodPost, "/stop", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://example.test")
	req.Host = "example.test"
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body:\n%s", rec.Code, rec.Body.String())
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/sounds?") {
		t.Errorf("Location = %q, want a return to /sounds", loc)
	}
	if !strings.Contains(loc, "stopped=1") {
		t.Errorf("Location = %q, want it to carry the stopped flash", loc)
	}
}

// The same host check /play's return value gets. A stop link is as good a
// carrier for an open redirect as a preview link.
func TestStopRefusesAnOffSiteReturn(t *testing.T) {
	srv, _, _ := newServerWithPlayer(t)
	setPassword(t, srv, "correct horse battery staple")
	cookie := login(t, srv, "correct horse battery staple")

	for _, bad := range []string{"http://evil.example/x", "//evil.example/x", `/\evil.example`} {
		form := url.Values{"return": {bad}}
		req := httptest.NewRequest(http.MethodPost, "/stop", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://example.test")
		req.Host = "example.test"
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)

		if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/?") {
			t.Errorf("return %q: Location = %q, want the / fallback", bad, loc)
		}
	}
}
