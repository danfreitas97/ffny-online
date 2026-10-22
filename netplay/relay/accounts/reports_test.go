package accounts

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func reportBody(t *testing.T, log, picture string) (*bytes.Buffer, string) {
	t.Helper()
	var b bytes.Buffer
	m := multipart.NewWriter(&b)
	m.WriteField("kind", "picture")
	m.WriteField("build", "18")
	f, _ := m.CreateFormFile("log", "run.log.err")
	f.Write([]byte(log))
	if picture != "" {
		f, _ = m.CreateFormFile("picture", "picture.png")
		f.Write([]byte(picture))
	}
	m.Close()
	return &b, m.FormDataContentType()
}

func sendReport(h http.Handler, body *bytes.Buffer, ctype, addr string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/api/report?format=text", body)
	req.Header.Set("Content-Type", ctype)
	req.Header.Set("X-Forwarded-For", addr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestBugReport(t *testing.T) {
	_, store, _ := newTestServer(t, discordUser{ID: "1", Username: "masa"})
	dir := filepath.Join(t.TempDir(), "reports")
	reports, err := OpenReports(dir)
	if err != nil {
		t.Fatal(err)
	}
	h := NewServer(Config{PublicURL: "https://play.example", Reports: reports}, store).Handler()

	body, ctype := reportBody(t, "[CRASH] sub_0010A7D0\n", "\x89PNG\r\n\x1a\nrest")
	rec := sendReport(h, body, ctype, "203.0.113.5")
	if rec.Code != http.StatusCreated || len(rec.Body.String()) != 6 {
		t.Fatalf("report: %d %q", rec.Code, rec.Body.String())
	}
	found, _ := filepath.Glob(filepath.Join(dir, "*-"+rec.Body.String()))
	if len(found) != 1 {
		t.Fatalf("report folder: %v", found)
	}
	logText, _ := os.ReadFile(filepath.Join(found[0], "log.txt"))
	info, _ := os.ReadFile(filepath.Join(found[0], "info.txt"))
	if string(logText) != "[CRASH] sub_0010A7D0\n" || !strings.Contains(string(info), "build: 18") ||
		strings.Contains(string(info), "203.0.113.5") {
		t.Fatalf("saved: %q / %q", logText, info)
	}
	if _, err := os.Stat(filepath.Join(found[0], "picture.png")); err != nil {
		t.Fatal("picture not saved")
	}

	// Not a PNG: the log is kept, the picture is not.
	body, ctype = reportBody(t, "x", "MZ\x90\x00 not a picture")
	rec = sendReport(h, body, ctype, "203.0.113.6")
	found, _ = filepath.Glob(filepath.Join(dir, "*-"+rec.Body.String(), "picture.png"))
	if rec.Code != http.StatusCreated || len(found) != 0 {
		t.Fatalf("non-PNG picture: %d %v", rec.Code, found)
	}

	// The rate limit is per address.
	for i := 1; i < ReportsPerHour; i++ {
		body, ctype = reportBody(t, "x", "")
		if rec = sendReport(h, body, ctype, "203.0.113.5"); rec.Code != http.StatusCreated {
			t.Fatalf("report %d: %d", i, rec.Code)
		}
	}
	body, ctype = reportBody(t, "x", "")
	if rec = sendReport(h, body, ctype, "203.0.113.5"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %d", rec.Code)
	}
	body, ctype = reportBody(t, "x", "")
	if rec = sendReport(h, body, ctype, "198.51.100.1"); rec.Code != http.StatusCreated {
		t.Fatalf("another address: %d", rec.Code)
	}
}

func TestBugReportOff(t *testing.T) {
	_, _, h := newTestServer(t, discordUser{ID: "1", Username: "masa"})
	body, ctype := reportBody(t, "x", "")
	if rec := sendReport(h, body, ctype, "203.0.113.5"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("reports off: %d", rec.Code)
	}
}
