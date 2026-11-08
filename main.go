// OllaLite: interfaz ultraligera para Ollama.
// Un solo binario, solo librería estándar de Go, UI embebida (HTML+JS sin frameworks).
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

const version = "1.0.0"

//go:embed ui/index.html
var rawIndex []byte

var (
	ollamaBase *url.URL
	homeDir    string
	defaultCwd string
	token      string
	page       []byte
	hosts      = map[string]bool{}
	origins    = map[string]bool{}
)

func main() {
	homeDir, _ = os.UserHomeDir()
	port := flag.Int("port", 11500, "puerto de la interfaz web (solo escucha en 127.0.0.1)")
	host := flag.String("ollama", os.Getenv("OLLAMA_HOST"), "dirección de Ollama (por defecto 127.0.0.1:11434)")
	dir := flag.String("dir", homeDir, "carpeta de trabajo por defecto para las tools")
	cli := flag.Bool("cli", false, "modo terminal: sin navegador, consumo mínimo")
	model := flag.String("model", "", "modelo inicial (modo -cli)")
	auto := flag.Bool("auto", false, "auto-aprobar escritura de archivos y comandos (modo -cli)")
	noOpen := flag.Bool("no-open", false, "no abrir el navegador al iniciar")
	appWin := flag.Bool("app", false, "abrir como ventana de app (Chrome/Edge/Brave --app)")
	showVer := flag.Bool("version", false, "mostrar la versión")
	flag.Parse()
	if *showVer {
		fmt.Println("ollalite", version)
		return
	}

	// GC más agresivo + techo blando de memoria: la app se mantiene en pocos MB
	// y deja toda la RAM posible para el modelo.
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(32 << 20)

	enrichPath()
	ollamaBase = parseOllamaHost(*host)
	defaultCwd = expandPath(*dir, homeDir)

	if *cli {
		runCLI(*model, *auto)
		return
	}
	serve(*port, !*noOpen, *appWin)
}

// ---------- servidor web ----------

func serve(port int, open, appWin bool) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	base := "http://" + addr
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		if alreadyRunning(base) {
			fmt.Println("OllaLite ya está abierto en", base)
			if open {
				openBrowser(base, appWin)
			}
			return
		}
		log.Fatalf("no se pudo abrir %s: %v (usa -port para otro puerto)", addr, err)
	}
	token = randHex(16)
	page = bytes.Replace(rawIndex, []byte("__TOKEN__"), []byte(token), 1)
	hosts[addr], hosts[fmt.Sprintf("localhost:%d", port)] = true, true
	origins[base], origins[fmt.Sprintf("http://localhost:%d", port)] = true, true

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(page)
	})
	mux.HandleFunc("GET /api/ping", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ollalite") })
	mux.Handle("/ollama/", guard(ollamaProxy()))
	mux.Handle("POST /api/chat", guard(http.HandlerFunc(handleChat)))
	mux.Handle("POST /api/approve", guard(http.HandlerFunc(handleApprove)))
	mux.Handle("POST /api/exec", guard(http.HandlerFunc(handleExec)))
	mux.Handle("GET /api/info", guard(http.HandlerFunc(handleInfo)))
	mux.Handle("POST /api/start-ollama", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		err := startOllama()
		writeJSON(w, map[string]any{"ok": err == nil, "error": errString(err)})
	})))
	mux.Handle("POST /api/quit", guard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok")
		go func() { time.Sleep(200 * time.Millisecond); os.Exit(0) }()
	})))

	srv := &http.Server{Handler: secure(mux), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 90 * time.Second}
	fmt.Printf("OllaLite %s → %s   (Ollama: %s · Ctrl+C para salir)\n", version, base, ollamaBase)
	if open {
		go openBrowser(base, appWin)
	}
	log.Fatal(srv.Serve(ln))
}

// secure bloquea DNS-rebinding (Host distinto) y el embebido en iframes.
func secure(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hosts[r.Host] {
			http.Error(w, "host no permitido", http.StatusForbidden)
			return
		}
		hd := w.Header()
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src 'self' data: blob:; connect-src 'self'; frame-ancestors 'none'")
		h.ServeHTTP(w, r)
	})
}

// guard exige el token por sesión (solo lo conoce la página servida) y origen propio:
// ninguna otra web abierta en el navegador puede usar la API para ejecutar comandos.
func guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && !origins[o] {
			http.Error(w, "origen no permitido", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Token")), []byte(token)) != 1 {
			http.Error(w, "token inválido", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ollamaProxy expone toda la API de Ollama en /ollama/* (list, ps, show, pull, push, cp, rm, generate, embed…).
func ollamaProxy() http.Handler {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = ollamaBase.Scheme
			pr.Out.URL.Host = ollamaBase.Host
			pr.Out.URL.Path = ollamaBase.Path + strings.TrimPrefix(pr.In.URL.Path, "/ollama")
			pr.Out.URL.RawPath = ""
			pr.Out.Host = ollamaBase.Host
			for _, h := range []string{"X-Token", "Origin", "Referer", "Cookie"} {
				pr.Out.Header.Del(h)
			}
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": "Ollama no responde en " + ollamaBase.String()})
		},
	}
}

// ---------- chat con tools (stream NDJSON) ----------

var (
	pendMu  sync.Mutex
	pending = map[string]chan bool{}
)

func handleChat(w http.ResponseWriter, r *http.Request) {
	var req ChatReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Cache-Control", "no-store")
	rc := http.NewResponseController(w)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	var mu sync.Mutex // el latido escribe desde otra goroutine
	emit := func(e Event) { mu.Lock(); enc.Encode(e); rc.Flush(); mu.Unlock() }

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	st := &Status{}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); heartbeat(ctx, stop, req.Model, st, emit, cancel) }()

	err := runAgent(ctx, &req, emit, webApprove, st)
	close(stop)
	wg.Wait()
	if err != nil && ctx.Err() == nil { // si ctx se canceló, el navegador cortó o el latido ya explicó el motivo
		emit(Event{T: "err", D: err.Error()})
	}
	emit(Event{T: "done"})
	debug.FreeOSMemory() // devolver al sistema lo usado durante la respuesta
}

func webApprove(ctx context.Context, id, _ string, _ json.RawMessage) bool {
	ch := make(chan bool, 1)
	pendMu.Lock()
	pending[id] = ch
	pendMu.Unlock()
	defer func() { pendMu.Lock(); delete(pending, id); pendMu.Unlock() }()
	select {
	case ok := <-ch:
		return ok
	case <-ctx.Done():
		return false
	}
}

func handleApprove(w http.ResponseWriter, r *http.Request) {
	var a struct {
		ID string `json:"id"`
		OK bool   `json:"ok"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&a); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pendMu.Lock()
	ch := pending[a.ID]
	pendMu.Unlock()
	if ch == nil {
		http.Error(w, "no hay ninguna acción pendiente con ese id", http.StatusNotFound)
		return
	}
	select {
	case ch <- a.OK:
	default:
	}
	io.WriteString(w, "ok")
}

// ---------- consola: ejecuta el binario `ollama` con cualquier subcomando ----------

func handleExec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Args      string `json:"args"`
		Modelfile string `json:"modelfile"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	args := splitArgs(req.Args)
	if len(args) > 0 && args[0] == "ollama" {
		args = args[1:]
	}
	if len(args) == 0 {
		http.Error(w, "escribe un comando, p. ej. list", http.StatusBadRequest)
		return
	}
	bin := ollamaBin()
	if bin == "" {
		http.Error(w, "no se encontró el ejecutable ollama en este equipo", http.StatusNotFound)
		return
	}
	if req.Modelfile != "" {
		f, err := os.CreateTemp("", "Modelfile-*")
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		f.WriteString(req.Modelfile)
		f.Close()
		defer os.Remove(f.Name())
		args = append(args, "-f", f.Name())
	}
	cmd := exec.CommandContext(r.Context(), bin, args...)
	cmd.Dir = defaultCwd
	cmd.Env = append(os.Environ(), "OLLAMA_HOST="+ollamaBase.String(), "NO_COLOR=1", "TERM=dumb")
	detach(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 2 * time.Second

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fw := &flushWriter{w: w, rc: http.NewResponseController(w)}
	cmd.Stdout, cmd.Stderr = fw, fw
	fmt.Fprintf(fw, "$ ollama %s\n", strings.Join(args, " "))
	if err := cmd.Run(); err != nil && r.Context().Err() == nil {
		fmt.Fprintf(fw, "\n[%v]\n", err)
	}
}

type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (f *flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	f.rc.Flush()
	return n, err
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	writeJSON(w, map[string]any{
		"version": version, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"home": homeDir, "cwd": defaultCwd, "shell": shellName(),
		"ollama": ollamaBase.String(), "ollama_bin": ollamaBin() != "",
		"rss_mb": rssMB(), "heap_mb": float64(ms.HeapInuse) / (1 << 20), "sys_mb": float64(ms.Sys) / (1 << 20),
	})
}

// ---------- utilidades ----------

func parseOllamaHost(h string) *url.URL {
	if h == "" {
		h = "127.0.0.1:11434"
	}
	if !strings.Contains(h, "://") {
		h = "http://" + h
	}
	u, err := url.Parse(h)
	if err != nil {
		log.Fatalf("OLLAMA_HOST inválido: %v", err)
	}
	hn, p := u.Hostname(), u.Port()
	if hn == "" || hn == "0.0.0.0" || hn == "::" {
		hn = "127.0.0.1"
	}
	if p == "" {
		p = "11434"
	}
	u.Host = net.JoinHostPort(hn, p)
	u.Path = strings.TrimRight(u.Path, "/")
	return u
}

// ollamaJSON hace una llamada simple (no streaming) a la API de Ollama.
func ollamaJSON(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, ollamaBase.String()+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func ollamaUp() bool {
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	return ollamaJSON(ctx, "GET", "/api/version", nil, nil) == nil
}

func ollamaBin() string {
	if p, err := exec.LookPath("ollama"); err == nil {
		return p
	}
	for _, p := range []string{"/usr/local/bin/ollama", "/opt/homebrew/bin/ollama", "/Applications/Ollama.app/Contents/Resources/ollama"} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// startOllama arranca el servidor de Ollama en segundo plano (sobrevive a OllaLite).
func startOllama() error {
	if ollamaUp() {
		return nil
	}
	var err error
	if _, e := os.Stat("/Applications/Ollama.app"); runtime.GOOS == "darwin" && e == nil {
		err = exec.Command("open", "-ga", "Ollama").Run()
	} else if bin := ollamaBin(); bin != "" {
		cmd := exec.Command(bin, "serve")
		detach(cmd)
		if err = cmd.Start(); err == nil {
			go cmd.Wait()
		}
	} else {
		err = errors.New("no se encontró ollama instalado (https://ollama.com/download)")
	}
	for i := 0; err == nil && i < 60 && !ollamaUp(); i++ {
		time.Sleep(250 * time.Millisecond)
	}
	if err == nil && !ollamaUp() {
		err = errors.New("Ollama no respondió a tiempo")
	}
	return err
}

func alreadyRunning(base string) bool {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(base + "/api/ping")
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	return string(b) == "ollalite"
}

func openBrowser(u string, app bool) {
	switch runtime.GOOS {
	case "darwin":
		if app {
			for _, b := range []string{"Google Chrome", "Microsoft Edge", "Brave Browser", "Chromium"} {
				if exec.Command("open", "-na", b, "--args", "--app="+u).Run() == nil {
					return
				}
			}
		}
		exec.Command("open", u).Start()
	case "windows":
		if app && exec.Command("cmd", "/C", "start", "msedge", "--app="+u).Run() == nil {
			return
		}
		exec.Command("rundll32", "url.dll,FileProtocolHandler", u).Start()
	default:
		if app {
			for _, b := range []string{"google-chrome", "chromium", "chromium-browser", "microsoft-edge", "brave-browser"} {
				if p, err := exec.LookPath(b); err == nil && exec.Command(p, "--app="+u).Start() == nil {
					return
				}
			}
		}
		exec.Command("xdg-open", u).Start()
	}
}

// enrichPath añade rutas típicas al PATH (útil si se abre desde Finder/.app con PATH mínimo).
func enrichPath() {
	if runtime.GOOS == "windows" {
		return
	}
	cur := os.Getenv("PATH")
	have := map[string]bool{}
	for _, p := range filepath.SplitList(cur) {
		have[p] = true
	}
	for _, p := range []string{"/opt/homebrew/bin", "/opt/homebrew/sbin", "/usr/local/bin", "/usr/bin", "/bin", "/usr/sbin", "/sbin",
		filepath.Join(homeDir, ".local/bin"), filepath.Join(homeDir, "go/bin"), filepath.Join(homeDir, ".cargo/bin")} {
		if !have[p] {
			if _, err := os.Stat(p); err == nil {
				cur += string(os.PathListSeparator) + p
			}
		}
	}
	os.Setenv("PATH", cur)
}

// splitArgs separa argumentos respetando comillas simples y dobles.
func splitArgs(s string) []string {
	var out []string
	var cur strings.Builder
	var q rune
	in := false
	for _, c := range s {
		switch {
		case q != 0:
			if c == q {
				q = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '"' || c == '\'':
			q, in = c, true
		case c == ' ' || c == '\t' || c == '\n':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
