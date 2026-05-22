package main

import (
	"crypto/rand"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

const (
	dataDir        = "data"
	idChars        = "abcdefghijklmnopqrstuvwxyz0123456789"
	idLen          = 6
	maxPasteKB     = 512
	rateLimitWindow = time.Hour
	rateLimitMax   = 30 // pastes per IP per window
)

type Paste struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Created string `json:"created"`
}

var (
	tmpl      *template.Template
	adminPass string

	// rate limiting
	rateMu    sync.Mutex
	rateMap   = make(map[string][]time.Time)
)

func main() {
	tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"splitLines": func(s string) []string { return strings.Split(s, "\n") },
		"trim":       func(s string, n int) string { if len(s) > n { return s[:n] + "..." }; return s },
	}).ParseFS(templatesFS, "templates/*.html"))

	adminPass = os.Getenv("ADMIN_PASSWORD")

	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("failed to create data dir: %v", err)
	}

	// periodic cleanup of rate limit map
	go func() {
		for range time.Tick(10 * time.Minute) {
			cleanupRateMap()
		}
	}()

	mux := http.NewServeMux()

	mux.HandleFunc("POST /api/paste", handleCreatePaste)
	mux.HandleFunc("DELETE /api/paste/{id}", handleDeletePaste)
	mux.HandleFunc("GET /admin", handleAdmin)
	mux.HandleFunc("GET /login", handleLogin)
	mux.HandleFunc("POST /login", handleLoginPost)
	mux.HandleFunc("GET /{id}", handleViewPaste)
	mux.HandleFunc("GET /raw/{id}", handleRawPaste)
	mux.HandleFunc("GET /", handleHome)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8079"
	}

	log.Printf("pastebin starting on :%s", port)
	if err := http.ListenAndServe(":"+port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func generateID() string {
	b := make([]byte, idLen)
	for i := range b {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(idChars))))
		b[i] = idChars[n.Int64()]
	}
	return string(b)
}

func pastePath(id string) string {
	return filepath.Join(dataDir, id+".json")
}

func savePaste(p Paste) error {
	f, err := os.Create(pastePath(p.ID))
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(p)
}

func loadPaste(id string) (*Paste, error) {
	f, err := os.Open(pastePath(id))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var p Paste
	if err := json.NewDecoder(f).Decode(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func listPastes() ([]Paste, error) {
	entries, err := os.ReadDir(dataDir)
	if err != nil {
		return nil, err
	}
	var pastes []Paste
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		p, err := loadPaste(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			continue
		}
		pastes = append(pastes, *p)
	}
	sort.Slice(pastes, func(i, j int) bool {
		return pastes[i].Created > pastes[j].Created
	})
	return pastes, nil
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		parts := strings.Split(fwd, ",")
		return strings.TrimSpace(parts[0])
	}
	if fwd := r.Header.Get("X-Real-IP"); fwd != "" {
		return fwd
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func checkRateLimit(ip string) bool {
	rateMu.Lock()
	defer rateMu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rateLimitWindow)

	times := rateMap[ip]
	var recent []time.Time
	for _, t := range times {
		if t.After(cutoff) {
			recent = append(recent, t)
		}
	}

	if len(recent) >= rateLimitMax {
		rateMap[ip] = recent
		return false
	}

	recent = append(recent, now)
	rateMap[ip] = recent
	return true
}

func cleanupRateMap() {
	rateMu.Lock()
	defer rateMu.Unlock()

	cutoff := time.Now().Add(-rateLimitWindow)
	for ip, times := range rateMap {
		var recent []time.Time
		for _, t := range times {
			if t.After(cutoff) {
				recent = append(recent, t)
			}
		}
		if len(recent) == 0 {
			delete(rateMap, ip)
		} else {
			rateMap[ip] = recent
		}
	}
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if adminPass == "" {
		http.Error(w, "admin not configured", http.StatusForbidden)
		return false
	}
	cookie, _ := r.Cookie("admin_token")
	if cookie != nil && cookie.Value == adminPass {
		return true
	}
	// redirect to login page
	http.Redirect(w, r, "/login?next="+r.URL.Path, http.StatusSeeOther)
	return false
}

// --- Handlers ---

func handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	render(w, "home.html", nil)
}

func handleCreatePaste(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !checkRateLimit(ip) {
		http.Error(w, "rate limited — too many pastes, try later", http.StatusTooManyRequests)
		return
	}

	var content string
	ct := r.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") {
		var req struct {
			Content string `json:"content"`
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxPasteKB*1024+1024))
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "invalid json", http.StatusBadRequest)
			return
		}
		content = req.Content
	} else {
		body, _ := io.ReadAll(io.LimitReader(r.Body, maxPasteKB*1024+1024))
		content = string(body)
	}

	if len(content) == 0 {
		http.Error(w, "empty paste", http.StatusBadRequest)
		return
	}
	if len(content) > maxPasteKB*1024 {
		http.Error(w, fmt.Sprintf("paste too large (max %dKB)", maxPasteKB), http.StatusRequestEntityTooLarge)
		return
	}

	p := Paste{
		ID:      generateID(),
		Content: content,
		Created: time.Now().UTC().Format(time.RFC3339),
	}

	if err := savePaste(p); err != nil {
		http.Error(w, "failed to save paste", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"id":  p.ID,
		"url": "/" + p.ID,
	})
}

func handleViewPaste(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	p, err := loadPaste(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	render(w, "paste.html", p)
}

func handleRawPaste(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.NotFound(w, r)
		return
	}
	p, err := loadPaste(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte(p.Content))
}

func handleDeletePaste(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	if err := os.Remove(pastePath(id)); err != nil {
		http.Error(w, "not found or delete failed", http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func handleAdmin(w http.ResponseWriter, r *http.Request) {
	if !requireAdmin(w, r) {
		return
	}
	pastes, err := listPastes()
	if err != nil {
		http.Error(w, "failed to list pastes", http.StatusInternalServerError)
		return
	}
	render(w, "admin.html", pastes)
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	// already logged in? redirect to admin
	cookie, _ := r.Cookie("admin_token")
	if cookie != nil && cookie.Value == adminPass {
		http.Redirect(w, r, "/admin", http.StatusSeeOther)
		return
	}
	render(w, "login.html", map[string]string{"Error": r.URL.Query().Get("error")})
}

func handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if r.FormValue("pass") != adminPass {
		http.Redirect(w, r, "/login?error=wrong+password", http.StatusSeeOther)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     "admin_token",
		Value:    adminPass,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   86400,
	})
	next := r.URL.Query().Get("next")
	if next == "" {
		next = "/admin"
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func render(w http.ResponseWriter, name string, data interface{}) {
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
