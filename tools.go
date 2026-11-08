package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxOut limita lo que cada tool devuelve al modelo (~3-4k tokens) para no llenar el contexto.
const maxOut = 12000

var toolSchemas = json.RawMessage(`[
{"type":"function","function":{"name":"read_file","description":"Read a text file. Returns numbered lines. Use offset/limit to page through long files.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"File path: absolute, ~/..., or relative to the working directory"},"offset":{"type":"integer","description":"First line to read (1-based). Default 1"},"limit":{"type":"integer","description":"Max lines to return. Default 400"}},"required":["path"]}}},
{"type":"function","function":{"name":"write_file","description":"Create a new file or completely overwrite an existing one. Parent folders are created automatically.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"File path"},"content":{"type":"string","description":"Full file content"}},"required":["path","content"]}}},
{"type":"function","function":{"name":"edit_file","description":"Replace an exact piece of text in an existing file. old_text must match the file exactly (including indentation) and be unique unless replace_all is true. Read the file first.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"File path"},"old_text":{"type":"string","description":"Exact text to replace"},"new_text":{"type":"string","description":"Replacement text"},"replace_all":{"type":"boolean","description":"Replace every occurrence. Default false"}},"required":["path","old_text","new_text"]}}},
{"type":"function","function":{"name":"list_dir","description":"List a folder's contents with sizes. depth > 1 also lists subfolders.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"Folder path. Default: working directory"},"depth":{"type":"integer","description":"1 to 4. Default 1"}}}}},
{"type":"function","function":{"name":"find_files","description":"Find files or folders anywhere under a folder (default: the user's whole home folder). Search by name with 'pattern' (substring like 'informe' or glob like '*.pdf'), and/or by words inside documents (PDF, Word, text...) with 'content'.","parameters":{"type":"object","properties":{"pattern":{"type":"string","description":"Name substring or glob, e.g. tesis, *.py, foto*2024*"},"content":{"type":"string","description":"Words that must appear inside the file (full-text search)"},"path":{"type":"string","description":"Folder to search in. Default: home"},"max_results":{"type":"integer","description":"Default 50"}}}}},
{"type":"function","function":{"name":"search_text","description":"Search inside text/code files for a regular expression, like grep. Returns path:line: text.","parameters":{"type":"object","properties":{"pattern":{"type":"string","description":"Regex (RE2 syntax) or plain text"},"path":{"type":"string","description":"Folder or file. Default: working directory"},"glob":{"type":"string","description":"Only file names matching this glob, e.g. *.js"},"ignore_case":{"type":"boolean"},"max_results":{"type":"integer","description":"Default 100"}},"required":["pattern"]}}},
{"type":"function","function":{"name":"run_command","description":"Run a shell command and return its output and exit code. Use it for git, builds, scripts, package managers, moving/copying/deleting files, system info, etc. Non-interactive.","parameters":{"type":"object","properties":{"command":{"type":"string","description":"Shell command line"},"cwd":{"type":"string","description":"Directory to run in. Default: working directory"},"timeout_sec":{"type":"integer","description":"Default 120, max 1800"}},"required":["command"]}}},
{"type":"function","function":{"name":"fetch_url","description":"Download a web page or text file with HTTP GET and return its readable text.","parameters":{"type":"object","properties":{"url":{"type":"string"}},"required":["url"]}}}
]`)

// Tools que modifican el sistema: piden aprobación salvo en modo auto.
var needApproval = map[string]bool{"write_file": true, "edit_file": true, "run_command": true}

func needsApproval(name string) bool { return needApproval[name] }

// flexInt/flexBool aceptan números y booleanos aunque el modelo los mande como string.
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `" `)
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		*f = flexInt(v)
	}
	return nil
}

type flexBool bool

func (f *flexBool) UnmarshalJSON(b []byte) error {
	s := strings.ToLower(strings.Trim(string(b), `" `))
	*f = s == "true" || s == "1" || s == "yes"
	return nil
}

type toolArgs struct {
	Path       string   `json:"path"`
	FilePath   string   `json:"file_path"`
	Directory  string   `json:"directory"`
	Content    string   `json:"content"`
	OldText    string   `json:"old_text"`
	NewText    string   `json:"new_text"`
	ReplaceAll flexBool `json:"replace_all"`
	Offset     flexInt  `json:"offset"`
	Limit      flexInt  `json:"limit"`
	Depth      flexInt  `json:"depth"`
	Pattern    string   `json:"pattern"`
	Query      string   `json:"query"`
	Glob       string   `json:"glob"`
	IgnoreCase flexBool `json:"ignore_case"`
	MaxResults flexInt  `json:"max_results"`
	Command    string   `json:"command"`
	Cwd        string   `json:"cwd"`
	TimeoutSec flexInt  `json:"timeout_sec"`
	URL        string   `json:"url"`
}

type toolCtx struct{ cwd string }

func (t *toolCtx) abs(p string) string { return expandPath(p, t.cwd) }

var errExit = errors.New("exit code != 0")

func runTool(ctx context.Context, t *toolCtx, name string, raw json.RawMessage) (out string, failed bool) {
	defer func() {
		if r := recover(); r != nil {
			out, failed = fmt.Sprintf("internal error: %v", r), true
		}
	}()
	var a toolArgs
	if err := json.Unmarshal(normArgs(raw), &a); err != nil {
		return "invalid arguments: " + err.Error(), true
	}
	if a.Path == "" {
		a.Path = a.FilePath
	}
	if a.Path == "" {
		a.Path = a.Directory
	}
	if a.Pattern == "" {
		a.Pattern = a.Query
	}
	var err error
	switch name {
	case "read_file":
		out, err = readFile(t.abs(a.Path), int(a.Offset), int(a.Limit))
	case "write_file":
		out, err = writeFile(t.abs(a.Path), a.Content)
	case "edit_file":
		out, err = editFile(t.abs(a.Path), a.OldText, a.NewText, bool(a.ReplaceAll))
	case "list_dir":
		out, err = listDir(t.abs(a.Path), int(a.Depth))
	case "find_files":
		root := homeDir
		if a.Path != "" {
			root = t.abs(a.Path)
		}
		out, err = findFiles(ctx, root, a.Pattern, a.Content, int(a.MaxResults))
	case "search_text":
		out, err = searchText(ctx, t.abs(a.Path), a.Pattern, a.Glob, bool(a.IgnoreCase), int(a.MaxResults))
	case "run_command":
		dir := t.cwd
		if a.Cwd != "" {
			dir = t.abs(a.Cwd)
		}
		out, err = runCommand(ctx, dir, a.Command, int(a.TimeoutSec))
	case "fetch_url":
		out, err = fetchURL(ctx, a.URL)
	default:
		return fmt.Sprintf("unknown tool %q. Available: read_file, write_file, edit_file, list_dir, find_files, search_text, run_command, fetch_url", name), true
	}
	if errors.Is(err, errExit) {
		return clip(out), true
	}
	if err != nil {
		return "error: " + err.Error(), true
	}
	return clip(out), false
}

// ---------- archivos ----------

func readFile(p string, offset, limit int) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("%s is a directory (use list_dir)", p)
	}
	br := bufio.NewReaderSize(f, 64<<10)
	if head, _ := br.Peek(8000); bytes.IndexByte(head, 0) >= 0 {
		return fmt.Sprintf("%s is a binary file (%s); it can't be shown as text.", p, human(st.Size())), nil
	}
	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = 400
	}
	if limit > 3000 {
		limit = 3000
	}
	var b strings.Builder
	n := 0
	for {
		line, rerr := br.ReadString('\n')
		if len(line) > 0 {
			n++
			if n >= offset {
				if n >= offset+limit || b.Len() > maxOut {
					fmt.Fprintf(&b, "... (more lines; continue with offset=%d)\n", n)
					break
				}
				line = strings.TrimRight(line, "\r\n")
				if len(line) > 2000 {
					line = line[:2000] + "…"
				}
				fmt.Fprintf(&b, "%5d\t%s\n", n, line)
			}
		}
		if rerr != nil {
			break
		}
	}
	if b.Len() == 0 {
		if n == 0 {
			return p + " is empty.", nil
		}
		return fmt.Sprintf("offset %d is past the end (the file has %d lines).", offset, n), nil
	}
	return fmt.Sprintf("%s (%s)\n%s", p, human(st.Size()), b.String()), nil
}

func writeFile(p, content string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	mode, verb := fs.FileMode(0o644), "Created"
	if st, err := os.Stat(p); err == nil {
		if st.IsDir() {
			return "", fmt.Errorf("%s is a directory", p)
		}
		mode, verb = st.Mode().Perm(), "Overwrote"
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s %s (%d lines, %s).", verb, p, strings.Count(content, "\n")+1, human(int64(len(content)))), nil
}

func editFile(p, old, nw string, all bool) (string, error) {
	if old == "" {
		return "", errors.New("old_text is empty; use write_file to create or overwrite a file")
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	s := string(data)
	n := strings.Count(s, old)
	if n == 0 && strings.Contains(s, "\r\n") && !strings.Contains(old, "\r") { // archivo con saltos Windows
		if o2 := strings.ReplaceAll(old, "\n", "\r\n"); strings.Count(s, o2) > 0 {
			old, nw, n = o2, strings.ReplaceAll(nw, "\n", "\r\n"), strings.Count(s, o2)
		}
	}
	if n == 0 {
		return "", errors.New("old_text was not found. Read the file again and copy the exact text, including indentation")
	}
	if n > 1 && !all {
		return "", fmt.Errorf("old_text appears %d times; include more surrounding lines to make it unique, or set replace_all=true", n)
	}
	if all {
		s = strings.ReplaceAll(s, old, nw)
	} else {
		s, n = strings.Replace(s, old, nw, 1), 1
	}
	if err := os.WriteFile(p, []byte(s), st.Mode().Perm()); err != nil {
		return "", err
	}
	return fmt.Sprintf("Edited %s: %d replacement(s).", p, n), nil
}

func listDir(root string, depth int) (string, error) {
	if depth < 1 {
		depth = 1
	}
	if depth > 4 {
		depth = 4
	}
	st, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !st.IsDir() {
		return fmt.Sprintf("%s is a file (%s).", root, human(st.Size())), nil
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s/\n", root)
	count, full := 0, false
	var walk func(dir string, lvl int)
	walk = func(dir string, lvl int) {
		ents, err := os.ReadDir(dir)
		ind := strings.Repeat("  ", lvl+1)
		if err != nil {
			fmt.Fprintf(&b, "%s[error: %v]\n", ind, err)
			return
		}
		sort.SliceStable(ents, func(i, j int) bool { return ents[i].IsDir() && !ents[j].IsDir() })
		for _, e := range ents {
			if count >= 500 || b.Len() > maxOut {
				full = true
				return
			}
			count++
			if e.IsDir() {
				fmt.Fprintf(&b, "%s%s/\n", ind, e.Name())
				if lvl+1 < depth && !skipDir(e.Name()) {
					walk(filepath.Join(dir, e.Name()), lvl+1)
				}
			} else if info, err := e.Info(); err == nil {
				fmt.Fprintf(&b, "%s%s  %s\n", ind, e.Name(), human(info.Size()))
			}
		}
	}
	walk(root, 0)
	if count == 0 {
		b.WriteString("  (empty)\n")
	}
	if full {
		b.WriteString("... (truncated)\n")
	}
	return b.String(), nil
}

// ---------- búsqueda ----------

var skipNames = map[string]bool{"node_modules": true, "Library": true, "__pycache__": true, "venv": true,
	"site-packages": true, "DerivedData": true, "Pods": true, "bower_components": true}
var skipSuffix = []string{".app", ".photoslibrary", ".musiclibrary", ".tvlibrary", ".xcarchive", ".framework", ".bundle", ".pkg", ".lproj"}

// skipDir: carpetas que no vale la pena recorrer (ocultas, dependencias, cachés, bundles).
func skipDir(name string) bool {
	if strings.HasPrefix(name, ".") || skipNames[name] {
		return true
	}
	for _, s := range skipSuffix {
		if strings.HasSuffix(name, s) {
			return true
		}
	}
	return false
}

// searchRoots añade iCloud Drive cuando se busca en todo el home (vive dentro de ~/Library).
func searchRoots(root string) []string {
	roots := []string{root}
	if root == homeDir {
		if icloud := filepath.Join(homeDir, "Library/Mobile Documents/com~apple~CloudDocs"); dirExists(icloud) {
			roots = append(roots, icloud)
		}
	}
	return roots
}

func findFiles(ctx context.Context, root, pattern, content string, max int) (string, error) {
	pattern, content = strings.TrimSpace(pattern), strings.TrimSpace(content)
	if pattern == "" && content == "" {
		return "", errors.New("give a name pattern and/or content words")
	}
	if !dirExists(root) {
		return "", fmt.Errorf("%s is not a folder", root)
	}
	if max <= 0 {
		max = 50
	}
	if max > 500 {
		max = 500
	}
	desc := fmt.Sprintf("name %q", pattern)
	if content != "" {
		desc = fmt.Sprintf("content %q", content)
		if pattern != "" {
			desc += fmt.Sprintf(" + name %q", pattern)
		}
	}
	// 1) Spotlight (macOS): instantáneo, indexa también el texto de PDF/Word/etc.
	if res, ok := spotlight(ctx, root, pattern, content, max); ok && (len(res) > 0 || content != "") {
		return formatFound(res, max, desc, root, "Spotlight"), nil
	}
	if content != "" {
		return "", errors.New("full-text search needs macOS Spotlight; use search_text for text/code files")
	}
	// 2) Recorrido propio del disco, con presupuesto de tiempo.
	glob := strings.ContainsAny(pattern, "*?[")
	pl := strings.ToLower(pattern)
	var res []string
	deadline := time.Now().Add(20 * time.Second)
	timedOut := false
	for _, r := range searchRoots(root) {
		filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				if d != nil && d.IsDir() && p != r {
					return fs.SkipDir
				}
				return nil
			}
			if ctx.Err() != nil || len(res) > max {
				return fs.SkipAll
			}
			if time.Now().After(deadline) {
				timedOut = true
				return fs.SkipAll
			}
			if p == r {
				return nil
			}
			name := strings.ToLower(d.Name())
			match := strings.Contains(name, pl)
			if glob {
				match, _ = filepath.Match(pl, name)
			}
			if match {
				if d.IsDir() {
					res = append(res, p+"/")
				} else {
					res = append(res, p)
				}
			}
			if d.IsDir() && skipDir(d.Name()) {
				return fs.SkipDir
			}
			return nil
		})
	}
	out := formatFound(res, max, desc, root, "disk scan")
	if timedOut {
		out += "\n(stopped after 20s; use a narrower path for a complete search)"
	}
	return out, nil
}

func spotlight(ctx context.Context, root, pattern, content string, max int) ([]string, bool) {
	if runtime.GOOS != "darwin" || strings.ContainsAny(pattern, "?[") {
		return nil, false
	}
	var q []string
	if pattern != "" {
		p := pattern
		if !strings.Contains(p, "*") {
			p = "*" + p + "*"
		}
		q = append(q, fmt.Sprintf(`kMDItemFSName == "%s"cd`, strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(p)))
	}
	if content != "" {
		for _, w := range strings.Fields(content) {
			q = append(q, fmt.Sprintf(`kMDItemTextContent == "%s"cd`, strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(w)))
		}
	}
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, "mdfind", "-onlyin", root, strings.Join(q, " && ")).Output()
	if err != nil {
		return nil, false
	}
	var res []string
	for _, l := range strings.Split(string(out), "\n") {
		if l == "" {
			continue
		}
		rel := strings.TrimPrefix(l, root)
		if strings.Contains(rel, "/.") || strings.Contains(rel, "/node_modules/") ||
			(strings.HasPrefix(rel, "/Library/") && !strings.HasPrefix(rel, "/Library/Mobile Documents/")) { // datos internos de apps
			continue
		}
		res = append(res, l)
		if len(res) > max {
			break
		}
	}
	return res, true
}

func formatFound(res []string, max int, desc, root, via string) string {
	if len(res) == 0 {
		return fmt.Sprintf("No files found for %s in %s.", desc, root)
	}
	more := ""
	if len(res) > max {
		res, more = res[:max], fmt.Sprintf("\n... (more than %d results; refine the search)", max)
	}
	return fmt.Sprintf("%d result(s) for %s in %s (via %s):\n%s%s", len(res), desc, root, via, strings.Join(res, "\n"), more)
}

var binExt = func() map[string]bool {
	m := map[string]bool{}
	for _, e := range strings.Fields(".png .jpg .jpeg .gif .webp .heic .ico .icns .pdf .zip .gz .tgz .bz2 .xz .7z .rar .tar .dmg .iso .pkg .exe .dll .so .dylib .o .a .class .jar .pyc .wasm .mp3 .mp4 .mov .avi .mkv .wav .flac .m4a .ttf .otf .woff .woff2 .db .sqlite .gguf .safetensors .bin .psd .sketch .fig .docx .xlsx .pptx .key .numbers .pages") {
		m[e] = true
	}
	return m
}()

func searchText(ctx context.Context, root, pattern, glob string, icase bool, max int) (string, error) {
	if pattern == "" {
		return "", errors.New("pattern is required")
	}
	if max <= 0 {
		max = 100
	}
	if max > 1000 {
		max = 1000
	}
	expr := pattern
	if _, err := regexp.Compile(expr); err != nil {
		expr = regexp.QuoteMeta(pattern) // si no es regex válida, buscar como texto literal
	}
	if icase {
		expr = "(?i)" + expr
	}
	re := regexp.MustCompile(expr)
	st, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	glob = strings.ToLower(glob)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var (
		mu    sync.Mutex
		hits  []string
		count atomic.Int32
		wg    sync.WaitGroup
	)
	files := make(chan string, 256)
	workers := min(runtime.NumCPU(), 8)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 64<<10)
			for p := range files {
				if ctx.Err() == nil && int(count.Load()) < max {
					grepFile(p, re, buf, max, &count, func(s string) { mu.Lock(); hits = append(hits, s); mu.Unlock() })
				}
			}
		}()
	}
	if !st.IsDir() {
		files <- root
	} else {
		for _, r := range searchRoots(root) {
			filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
				if err != nil {
					if d != nil && d.IsDir() && p != r {
						return fs.SkipDir
					}
					return nil
				}
				if ctx.Err() != nil || int(count.Load()) >= max {
					return fs.SkipAll
				}
				if d.IsDir() {
					if p != r && skipDir(d.Name()) {
						return fs.SkipDir
					}
					return nil
				}
				if !d.Type().IsRegular() {
					return nil
				}
				name := strings.ToLower(d.Name())
				if binExt[filepath.Ext(name)] {
					return nil
				}
				if glob != "" {
					if ok, _ := filepath.Match(glob, name); !ok {
						return nil
					}
				}
				select {
				case files <- p:
				case <-ctx.Done():
					return fs.SkipAll
				}
				return nil
			})
		}
	}
	close(files)
	wg.Wait()

	sort.Strings(hits)
	more := ""
	if len(hits) >= max {
		hits, more = hits[:min(len(hits), max)], "\n... (result limit reached; narrow the path or pattern)"
	}
	if ctx.Err() == context.DeadlineExceeded {
		more += "\n(search stopped after 30s; results may be incomplete)"
	}
	if len(hits) == 0 {
		return fmt.Sprintf("No matches for /%s/ in %s.%s", pattern, root, more), nil
	}
	return fmt.Sprintf("%d match(es) for /%s/ in %s:\n%s%s", len(hits), pattern, root, strings.Join(hits, "\n"), more), nil
}

func grepFile(p string, re *regexp.Regexp, buf []byte, max int, count *atomic.Int32, add func(string)) {
	f, err := os.Open(p)
	if err != nil {
		return
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > 8<<20 {
		return
	}
	br := bufio.NewReaderSize(f, 32<<10)
	if head, _ := br.Peek(1024); bytes.IndexByte(head, 0) >= 0 {
		return // binario
	}
	sc := bufio.NewScanner(br)
	sc.Buffer(buf, 1<<20)
	ln, perFile := 0, 0
	for sc.Scan() {
		ln++
		line := sc.Bytes()
		if !re.Match(line) {
			continue
		}
		if int(count.Add(1)) > max {
			return
		}
		s := strings.TrimSpace(string(line))
		if len(s) > 240 {
			s = s[:240] + "…"
		}
		add(fmt.Sprintf("%s:%d: %s", p, ln, s))
		if perFile++; perFile >= 20 {
			return
		}
	}
}

// ---------- comandos ----------

func runCommand(ctx context.Context, dir, command string, timeout int) (string, error) {
	if strings.TrimSpace(command) == "" {
		return "", errors.New("command is empty")
	}
	if !dirExists(dir) {
		return "", fmt.Errorf("directory %s does not exist", dir)
	}
	if timeout <= 0 {
		timeout = 120
	}
	if timeout > 1800 {
		timeout = 1800
	}
	cctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	cmd := shellCmd(cctx, command)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "TERM=dumb", "NO_COLOR=1", "PAGER=cat", "GIT_PAGER=cat", "GIT_TERMINAL_PROMPT=0")
	var out capBuf
	cmd.Stdout, cmd.Stderr = &out, &out
	detach(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 3 * time.Second
	start := time.Now()
	err := cmd.Run()
	if err != nil && cmd.ProcessState == nil {
		return "", err // no se pudo iniciar
	}
	code := cmd.ProcessState.ExitCode()
	res := strings.TrimRight(out.String(), "\n")
	if res == "" {
		res = "(no output)"
	}
	s := fmt.Sprintf("exit code %d · %.1fs\n%s", code, time.Since(start).Seconds(), res)
	if cctx.Err() == context.DeadlineExceeded {
		return s + fmt.Sprintf("\n[killed: timeout after %ds]", timeout), errExit
	}
	if code != 0 {
		return s, errExit
	}
	return s, nil
}

// capBuf guarda solo el inicio y el final de la salida: memoria acotada aunque el comando imprima GBs.
type capBuf struct {
	head, tail []byte
	dropped    int
}

const capHead, capTail = 3000, 9000

func (c *capBuf) Write(p []byte) (int, error) {
	n := len(p)
	if room := capHead - len(c.head); room > 0 {
		k := min(room, len(p))
		c.head = append(c.head, p[:k]...)
		p = p[k:]
	}
	if len(p) > 0 {
		c.tail = append(c.tail, p...)
		if len(c.tail) > 2*capTail {
			cut := len(c.tail) - capTail
			c.dropped += cut
			c.tail = c.tail[:copy(c.tail, c.tail[cut:])]
		}
	}
	return n, nil
}

func (c *capBuf) String() string {
	tail, dropped := c.tail, c.dropped
	if len(tail) > capTail {
		dropped += len(tail) - capTail
		tail = tail[len(tail)-capTail:]
	}
	if dropped > 0 {
		return string(c.head) + fmt.Sprintf("\n…[%d bytes omitted]…\n", dropped) + string(tail)
	}
	return string(c.head) + string(tail)
}

// ---------- web ----------

var (
	reComment = regexp.MustCompile(`(?s)<!--.*?-->`)
	reDrop    = regexp.MustCompile(`(?is)<(script|style|noscript|svg|template|iframe)\b.*?</(script|style|noscript|svg|template|iframe)\s*>`)
	reBlock   = regexp.MustCompile(`(?i)<(br|/p|/div|/li|/h[1-6]|/tr|/section|/article|/header|/footer|/pre|/blockquote|/table)\b[^>]*>`)
	reTag     = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces  = regexp.MustCompile(`[ \t\r\f\v]+`)
	reLines   = regexp.MustCompile(`\n(\s*\n)+`)
)

func fetchURL(ctx context.Context, u string) (string, error) {
	u = strings.TrimSpace(u)
	if u == "" {
		return "", errors.New("url is required")
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, "GET", u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; OllaLite/"+version+")")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 3<<20))
	ct := resp.Header.Get("Content-Type")
	head := data[:min(len(data), 2048)]
	var text string
	switch {
	case strings.Contains(ct, "html") || bytes.Contains(bytes.ToLower(head), []byte("<html")):
		text = htmlToText(string(data))
	case bytes.IndexByte(head, 0) >= 0:
		text = fmt.Sprintf("(binary content, %s)", human(int64(len(data))))
	default:
		text = string(data)
	}
	return fmt.Sprintf("%s → HTTP %d (%s)\n\n%s", u, resp.StatusCode, ct, text), nil
}

func htmlToText(s string) string {
	s = reComment.ReplaceAllString(s, "")
	s = reDrop.ReplaceAllString(s, "")
	s = reBlock.ReplaceAllString(s, "\n")
	s = reTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	s = reLines.ReplaceAllString(s, "\n\n")
	return strings.TrimSpace(s)
}

// ---------- utilidades ----------

func expandPath(p, base string) string {
	if base == "" {
		base = homeDir
	}
	p = strings.TrimSpace(p)
	switch {
	case p == "":
		return base
	case p == "~":
		return homeDir
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(homeDir, p[2:])
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(base, p)
	}
	return filepath.Clean(p)
}

func dirExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func clip(s string) string {
	if len(s) <= maxOut+500 {
		return s
	}
	return s[:maxOut] + fmt.Sprintf("\n…[truncated: %d more characters]", len(s)-maxOut)
}

func human(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	f, i := float64(n), -1
	for f >= 1024 && i < 4 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %cB", f, "KMGTP"[i])
}

func tildify(p string) string {
	if homeDir != "" && strings.HasPrefix(p, homeDir) {
		return "~" + strings.TrimPrefix(p, homeDir)
	}
	return p
}

// argSummary resume los argumentos de una tool en una línea (para la terminal).
func argSummary(name string, raw json.RawMessage) string {
	var a map[string]any
	json.Unmarshal(normArgs(raw), &a)
	get := func(k string) string {
		if v, ok := a[k]; ok && v != nil {
			return fmt.Sprint(v)
		}
		return ""
	}
	var s string
	switch name {
	case "run_command":
		s = get("command")
	case "find_files", "search_text":
		s = strings.TrimSpace(get("pattern") + " " + get("content"))
		if p := get("path"); p != "" {
			s += "  en " + p
		}
	case "fetch_url":
		s = get("url")
	default:
		s = get("path")
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 140 {
		s = s[:140] + "…"
	}
	return s
}
