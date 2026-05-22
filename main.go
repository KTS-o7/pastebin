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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

//go:embed templates/*.html
var templatesFS embed.FS

const (
	dataDir    = "data"
	idChars    = "abcdefghijklmnopqrstuvwxyz0123456789"
	idLen      = 6
	maxPasteKB = 512
)

type Paste struct {
	ID      string `json:"id"`
	Content string `json:"content"`
	Created string `json:"created"`
}

var tmpl *template.Template

func main() {
	// Parse templates with func map
	tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"splitLines": func(s string) []string { return strings.Split(s, "\n") },
	}).ParseFS(templatesFS, "templates/*.html"))

	// Ensure data dir exists
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		log.Fatalf("failed to create data dir: %v", err)
	}

	mux := http.NewServeMux()

	// Static path for API
	mux.HandleFunc("POST /api/paste", handleCreatePaste)
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

func handleHome(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	render(w, "home.html", nil)
}

func handleCreatePaste(w http.ResponseWriter, r *http.Request) {
	// Accept JSON or form-encoded
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

func render(w http.ResponseWriter, name string, data interface{}) {
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("template error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}
