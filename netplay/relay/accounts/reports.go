package accounts

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"log"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Bug reports: F9 in the game (or the launcher after a crash) sends the game's log and a
// picture of the screen, so the developers can read what went wrong without asking the player
// to find a file. POST /api/report takes multipart/form-data:
//
//	kind     "picture" (F9) or "crash"
//	build    the game's build number
//	log      the run log (the game cuts it to a head and a tail and blanks the Windows user folder)
//	picture  a PNG of the screen (optional)
//
// Each report becomes a folder <dir>/<YYYYMMDD-HHMMSS.mmm>-<id>/ with info.txt, log.txt and
// picture.png. The sender's address is used for the rate limit only and is not written down.

// Report limits.
const (
	MaxReportBytes   = 8 << 20 // one request
	ReportsPerHour   = 6       // per address
	ReportsPerDay    = 300     // all players together
	MaxReportsOnDisk = 2 << 30 // the folder; full means no new reports until someone clears it
)

var reportKind = regexp.MustCompile(`^[a-z]{1,16}$`)
var reportBuild = regexp.MustCompile(`^[0-9]{1,6}$`)

// Reports keeps the bug reports in a folder.
type Reports struct {
	dir    string
	mu     sync.Mutex
	recent map[string][]time.Time // address -> report times in the last hour
	day    []time.Time
	used   int64
}

// OpenReports makes the folder if needed and counts what is in it.
func OpenReports(dir string) (*Reports, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	r := &Reports{dir: dir, recent: map[string][]time.Time{}}
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				r.used += info.Size()
			}
		}
		return nil
	})
	return r, nil
}

// allow says whether this address may send a report now, and counts it if so.
func (r *Reports) allow(addr string, now time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	keep := func(ts []time.Time, span time.Duration) []time.Time {
		out := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < span {
				out = append(out, t)
			}
		}
		return out
	}
	for a, ts := range r.recent {
		if ts = keep(ts, time.Hour); len(ts) == 0 {
			delete(r.recent, a)
		} else {
			r.recent[a] = ts
		}
	}
	r.day = keep(r.day, 24*time.Hour)
	if len(r.recent[addr]) >= ReportsPerHour || len(r.day) >= ReportsPerDay || r.used >= MaxReportsOnDisk {
		return false
	}
	r.recent[addr] = append(r.recent[addr], now)
	r.day = append(r.day, now)
	return true
}

// clientAddr is the player's address: the proxy (Caddy) puts it last in X-Forwarded-For.
func clientAddr(req *http.Request) string {
	if f := req.Header.Get("X-Forwarded-For"); f != "" {
		parts := strings.Split(f, ",")
		return strings.TrimSpace(parts[len(parts)-1])
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

func (s *Server) bugReport(w http.ResponseWriter, r *http.Request) {
	if s.reports == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "reports are off"})
		return
	}
	now := s.now()
	if !s.reports.allow(clientAddr(r), now) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many reports, try again later"})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, MaxReportBytes)
	if err := r.ParseMultipartForm(MaxReportBytes); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "not a report"})
		return
	}
	defer r.MultipartForm.RemoveAll()
	kind, build := r.FormValue("kind"), r.FormValue("build")
	if !reportKind.MatchString(kind) {
		kind = "unknown"
	}
	if !reportBuild.MatchString(build) {
		build = "?"
	}
	logFile, _, err := r.FormFile("log")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no log"})
		return
	}
	defer logFile.Close()

	var idb [3]byte
	rand.Read(idb[:])
	id := hex.EncodeToString(idb[:])
	dir := filepath.Join(s.reports.dir, now.UTC().Format("20060102-150405.000")+"-"+id) // sorts by time
	if err := os.Mkdir(dir, 0o700); err != nil {
		log.Printf("accounts: report: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "not saved"})
		return
	}
	size := int64(0)
	save := func(name string, f multipart.File) {
		out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return
		}
		n, _ := io.Copy(out, f)
		out.Close()
		size += n
	}
	save("log.txt", logFile)
	if pic, _, err := r.FormFile("picture"); err == nil {
		if head := make([]byte, 8); func() bool { n, _ := io.ReadFull(pic, head); return n == 8 }() &&
			string(head) == "\x89PNG\r\n\x1a\n" {
			pic.Seek(0, io.SeekStart)
			save("picture.png", pic)
		}
		pic.Close()
	}
	who := "not signed in"
	if a, ok := s.store.Session(bearer(r), now); ok {
		who = fmt.Sprintf("account %d, %s (@%s)", a.ID, a.Name, a.Username)
	}
	info := fmt.Sprintf("id: %s\ntime: %s\nkind: %s\nbuild: %s\nplayer: %s\n", id, now.UTC().Format(time.RFC3339),
		kind, build, who)
	os.WriteFile(filepath.Join(dir, "info.txt"), []byte(info), 0o600)
	s.reports.mu.Lock()
	s.reports.used += size + int64(len(info))
	s.reports.mu.Unlock()
	log.Printf("accounts: bug report %s (%s, build %s, %d bytes)", id, kind, build, size)
	if r.URL.Query().Get("format") == "text" { // for the game: no JSON parser in C
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusCreated)
		io.WriteString(w, id)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id})
}

// ReportInfo is one report as the Discord bot lists it (Internal: GET /internal/reports).
type ReportInfo struct {
	Folder  string `json:"folder"`
	ID      string `json:"id"`
	Time    string `json:"time"`
	Kind    string `json:"kind"`
	Build   string `json:"build"`
	Player  string `json:"player"`
	Picture bool   `json:"picture"`
	Crash   string `json:"crash,omitempty"` // the log's first [CRASH] line and where, for crash reports
}

var reportFolder = regexp.MustCompile(`^[0-9]{8}-[0-9]{6}(\.[0-9]{3})?-[0-9a-f]{6}$`)

// After lists up to n reports whose folder sorts after the given one, oldest first.
func (r *Reports) After(after string, n int) []ReportInfo {
	entries, err := os.ReadDir(r.dir)
	if err != nil {
		return nil
	}
	var out []ReportInfo
	for _, e := range entries { // ReadDir sorts by name: by time
		name := e.Name()
		if !e.IsDir() || !reportFolder.MatchString(name) || name <= after {
			continue
		}
		info := ReportInfo{Folder: name, ID: name[len(name)-6:]}
		if b, err := os.ReadFile(filepath.Join(r.dir, name, "info.txt")); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				k, v, ok := strings.Cut(line, ": ")
				if !ok {
					continue
				}
				switch k {
				case "time":
					info.Time = v
				case "kind":
					info.Kind = v
				case "build":
					info.Build = v
				case "player":
					info.Player = v
				}
			}
		}
		if _, err := os.Stat(filepath.Join(r.dir, name, "picture.png")); err == nil {
			info.Picture = true
		}
		info.Crash = crashLine(filepath.Join(r.dir, name, "log.txt"))
		out = append(out, info)
		if len(out) >= n {
			break
		}
	}
	return out
}

// crashLine is the log's first "[CRASH]" line and the "in sub_..." line after it, if any.
func crashLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	text := string(b)
	i := strings.Index(text, "[CRASH]")
	if i < 0 {
		return ""
	}
	rest := text[i:]
	first, rest, _ := strings.Cut(rest, "\n")
	if j := strings.Index(rest, "  in sub_"); j >= 0 && j < 4000 {
		where, _, _ := strings.Cut(rest[j+2:], "\n")
		first += " | " + where
	}
	if len(first) > 300 {
		first = first[:300]
	}
	return first
}

// Picture is the path of a report's picture, or "" for a folder that is not a report.
func (r *Reports) Picture(folder string) string {
	if !reportFolder.MatchString(folder) {
		return ""
	}
	return filepath.Join(r.dir, folder, "picture.png")
}
