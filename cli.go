package main

// Modo terminal (-cli): sin navegador ni servidor HTTP. Es la opción de menor consumo.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	cReset  = "\x1b[0m"
	cDim    = "\x1b[2m"
	cBold   = "\x1b[1m"
	cRed    = "\x1b[31m"
	cGreen  = "\x1b[32m"
	cYellow = "\x1b[33m"
	cCyan   = "\x1b[36m"
)

type cli struct {
	model              string
	history            []Message
	auto, tools, think bool
	canTools, canThink bool
	numCtx             int
	cwd, system        string
	in                 *bufio.Reader
	color, inThink     bool
}

func runCLI(model string, auto bool) {
	c := &cli{model: model, auto: auto, tools: true, numCtx: 8192, cwd: defaultCwd, in: bufio.NewReaderSize(os.Stdin, 64<<10)}
	if fi, err := os.Stdout.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 && os.Getenv("NO_COLOR") == "" {
		c.color = true
	}
	if !ollamaUp() {
		fmt.Println(c.paint(cRed, "⚠ Ollama no responde en "+ollamaBase.String()+" — ábrelo o escribe /serve"))
	}
	if c.model == "" {
		c.model = firstModel()
	}
	c.loadCaps()
	c.banner()

	// Ctrl+C: cancela lo que esté en curso; si no hay nada, sale.
	var mu sync.Mutex
	var cancel context.CancelFunc
	setCancel := func(f context.CancelFunc) { mu.Lock(); cancel = f; mu.Unlock() }
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	go func() {
		for range sig {
			mu.Lock()
			f := cancel
			mu.Unlock()
			if f == nil {
				fmt.Println()
				os.Exit(0)
			}
			f()
		}
	}()

	for {
		fmt.Print(c.paint(cBold+cCyan, "\n› "))
		line, err := c.readInput()
		if err != nil {
			fmt.Println()
			return
		}
		line = strings.TrimSpace(line)
		switch {
		case line == "":
		case strings.HasPrefix(line, "!"):
			setCancel(func() {}) // el Ctrl+C lo recibe el proceso hijo
			c.shell(line[1:])
			setCancel(nil)
		case strings.HasPrefix(line, "/"):
			setCancel(func() {})
			quit := c.command(line[1:])
			setCancel(nil)
			if quit {
				return
			}
		default:
			ctx, cf := context.WithCancel(context.Background())
			setCancel(cf)
			c.chat(ctx, line)
			setCancel(nil)
			cf()
		}
	}
}

func (c *cli) chat(ctx context.Context, text string) {
	c.history = append(c.history, Message{Role: "user", Content: text})
	req := ChatReq{Model: c.model, Messages: c.history, System: c.system, Tools: c.tools && c.canTools,
		Auto: c.auto, Cwd: c.cwd, Options: json.RawMessage(fmt.Sprintf(`{"num_ctx":%d}`, c.numCtx))}
	if c.canThink {
		req.Think = json.RawMessage(strconv.FormatBool(c.think))
	}
	fmt.Println()
	err := runAgent(ctx, &req, c.emit, c.approve, &Status{})
	c.endThink()
	if err != nil {
		if ctx.Err() != nil {
			fmt.Print(c.paint(cDim, "\n[cancelado]"))
		} else {
			fmt.Print(c.paint(cRed, "\n"+err.Error()))
		}
	}
	fmt.Println()
}

func (c *cli) emit(e Event) {
	switch e.T {
	case "think":
		if !c.inThink {
			c.inThink = true
			fmt.Print(c.paint(cDim, "💭 "))
		}
		fmt.Print(c.paint(cDim, e.D))
	case "tok":
		c.endThink()
		fmt.Print(e.D)
	case "stats":
		c.endThink()
		if e.S != nil && e.S.EvalMs > 0 {
			fmt.Print(c.paint(cDim, fmt.Sprintf("\n  %.1f tok/s · %d tokens · %.1fs",
				float64(e.S.Eval)/(float64(e.S.EvalMs)/1000), e.S.Eval, float64(e.S.TotalMs)/1000)))
		}
	case "msg":
		if e.M != nil {
			c.history = append(c.history, *e.M)
		}
	case "call":
		c.endThink()
		fmt.Printf("\n%s %s %s", c.paint(cYellow, "⚙"), c.paint(cBold, e.Name), c.paint(cDim, argSummary(e.Name, e.Args)))
		if e.Need {
			c.preview(e.Name, e.Args)
		}
	case "result":
		if e.M != nil {
			c.history = append(c.history, *e.M)
			mark := c.paint(cGreen, "✓")
			if e.Err {
				mark = c.paint(cRed, "✗")
			}
			fmt.Printf("\n  %s %s\n", mark, c.paint(cDim, strings.ReplaceAll(firstLines(e.M.Content, 6), "\n", "\n    ")))
		}
	case "warn":
		fmt.Print(c.paint(cYellow, "\n⚠ "+e.D))
	case "err":
		fmt.Print(c.paint(cRed, "\n"+e.D))
	}
}

func (c *cli) endThink() {
	if c.inThink {
		c.inThink = false
		fmt.Print("\n\n")
	}
}

func (c *cli) preview(name string, raw json.RawMessage) {
	var a map[string]any
	json.Unmarshal(normArgs(raw), &a)
	s := func(k string) string { v, _ := a[k].(string); return v }
	switch name {
	case "write_file":
		fmt.Print("\n" + c.paint(cDim, prefixLines(firstLines(s("content"), 12), "    │ ")))
	case "edit_file":
		fmt.Print("\n" + c.paint(cRed, prefixLines(firstLines(s("old_text"), 8), "    - ")))
		fmt.Print("\n" + c.paint(cGreen, prefixLines(firstLines(s("new_text"), 8), "    + ")))
	case "run_command":
		if d := s("cwd"); d != "" {
			fmt.Print(c.paint(cDim, "   (en "+d+")"))
		}
	}
}

func (c *cli) approve(ctx context.Context, _, _ string, _ json.RawMessage) bool {
	if c.auto {
		return true
	}
	fmt.Print(c.paint(cYellow, "\n  ¿Permitir? [s]í / [n]o / [t]odo: "))
	line, err := c.in.ReadString('\n')
	if err != nil || ctx.Err() != nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "s", "si", "sí", "y", "yes":
		return true
	case "t", "todo", "a", "all":
		c.auto = true
		fmt.Print(c.paint(cDim, "  (auto-aprobar activado)"))
		return true
	}
	return false
}

// readInput lee una línea; los bloques entre """ … """ pueden tener varias líneas.
func (c *cli) readInput() (string, error) {
	line, err := c.in.ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	t := strings.TrimSpace(line)
	if !strings.HasPrefix(t, `"""`) {
		return line, nil
	}
	rest := strings.TrimPrefix(t, `"""`)
	if i := strings.Index(rest, `"""`); i >= 0 {
		return rest[:i], nil
	}
	var b strings.Builder
	b.WriteString(rest + "\n")
	for {
		l, err := c.in.ReadString('\n')
		if i := strings.Index(l, `"""`); i >= 0 {
			b.WriteString(l[:i])
			break
		}
		b.WriteString(l)
		if err != nil {
			break
		}
	}
	return b.String(), nil
}

func (c *cli) command(line string) (quit bool) {
	f := splitArgs(line)
	if len(f) == 0 {
		return false
	}
	cmd, args := strings.ToLower(f[0]), f[1:]
	arg := strings.Join(args, " ")
	withModel := func() []string {
		if len(args) == 0 && c.model != "" {
			return []string{c.model}
		}
		return args
	}
	switch cmd {
	case "exit", "quit", "q", "salir":
		return true
	case "help", "h", "?", "ayuda":
		c.help()
	case "use", "model", "modelo":
		if arg == "" {
			fmt.Println("modelo actual:", c.paint(cBold, c.model))
			runOllama("list")
			break
		}
		c.model = arg
		c.loadCaps()
		c.status()
	case "models", "list", "ls":
		runOllama("list")
	case "clear", "new", "nuevo":
		c.history = nil
		fmt.Println("conversación nueva")
	case "auto":
		c.auto = !c.auto
		c.status()
	case "tools":
		c.tools = !c.tools
		c.status()
	case "think", "pensar":
		c.think = !c.think
		c.status()
	case "cd":
		d := expandPath(arg, c.cwd)
		if !dirExists(d) {
			fmt.Println(c.paint(cRed, "no existe: "+d))
			break
		}
		c.cwd = d
		c.status()
	case "ctx":
		if n, err := strconv.Atoi(arg); err == nil && n >= 512 {
			c.numCtx = n
		}
		c.status()
	case "system":
		c.system = arg
		fmt.Println("instrucciones extra actualizadas")
	case "status", "estado":
		c.status()
	case "unload", "free", "liberar":
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := ollamaJSON(ctx, "POST", "/api/generate", map[string]any{"model": c.model, "keep_alive": 0}, nil); err != nil {
			fmt.Println(c.paint(cRed, err.Error()))
		} else {
			fmt.Println(c.model, "liberado de la RAM")
		}
	case "serve":
		if err := startOllama(); err != nil {
			fmt.Println(c.paint(cRed, err.Error()))
		} else {
			fmt.Println("Ollama listo en", ollamaBase)
		}
	case "ollama", "o":
		runOllama(args...)
	case "version":
		runOllama("--version")
	case "show", "stop", "run":
		runOllama(append([]string{cmd}, withModel()...)...)
	case "create", "pull", "push", "signin", "signout", "ps", "cp", "rm", "launch":
		runOllama(append([]string{cmd}, args...)...)
		if cmd == "pull" || cmd == "rm" || cmd == "cp" || cmd == "create" {
			c.loadCaps()
		}
	default:
		fmt.Println("comando desconocido; escribe /help")
	}
	return false
}

// runOllama ejecuta el CLI oficial de ollama conectado a esta terminal (los interactivos funcionan).
func runOllama(args ...string) {
	bin := ollamaBin()
	if bin == "" {
		fmt.Println("no se encontró el ejecutable ollama")
		return
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "OLLAMA_HOST="+ollamaBase.String())
	if err := cmd.Run(); err != nil {
		fmt.Println(err)
	}
}

func (c *cli) shell(line string) {
	cmd := shellCmd(context.Background(), line)
	cmd.Dir = c.cwd
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Println(c.paint(cDim, err.Error()))
	}
}

func (c *cli) loadCaps() {
	c.canTools, c.canThink = false, false
	if c.model == "" {
		fmt.Println(c.paint(cYellow, "No hay modelos instalados. Descarga uno con: /pull qwen3:8b"))
		return
	}
	var r struct {
		Capabilities []string `json:"capabilities"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := ollamaJSON(ctx, "POST", "/api/show", map[string]string{"model": c.model}, &r); err != nil {
		fmt.Println(c.paint(cRed, "⚠ "+c.model+": "+err.Error()))
		return
	}
	for _, x := range r.Capabilities {
		switch x {
		case "tools":
			c.canTools = true
		case "thinking":
			c.canThink = true
		}
	}
}

func firstModel() string {
	var r struct {
		Models []struct {
			Name         string   `json:"name"`
			Capabilities []string `json:"capabilities"`
		} `json:"models"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ollamaJSON(ctx, "GET", "/api/tags", nil, &r)
	for _, m := range r.Models {
		for _, cp := range m.Capabilities {
			if cp == "tools" {
				return m.Name
			}
		}
	}
	if len(r.Models) > 0 {
		return r.Models[0].Name
	}
	return ""
}

func (c *cli) banner() {
	fmt.Println(c.paint(cBold, "🍲 OllaLite") + c.paint(cDim, " "+version+" · modo terminal"))
	c.status()
	fmt.Println(c.paint(cDim, "Escribe tu mensaje · /help comandos · !cmd shell · Ctrl+C cancela"))
}

func (c *cli) status() {
	on := func(b bool) string {
		if b {
			return c.paint(cGreen, "on")
		}
		return c.paint(cDim, "off")
	}
	tools, think := c.paint(cDim, "n/a"), c.paint(cDim, "n/a")
	if c.canTools {
		tools = on(c.tools)
	}
	if c.canThink {
		think = on(c.think)
	}
	fmt.Printf("modelo %s · tools %s · pensar %s · auto %s · ctx %d · carpeta %s\n",
		c.paint(cBold, c.model), tools, think, on(c.auto), c.numCtx, tildify(c.cwd))
}

func (c *cli) help() {
	fmt.Print(`
Chat: escribe y pulsa Enter. Para varias líneas usa """ … """.
  !<comando>        ejecuta un comando de shell directamente
  Ctrl+C            cancela la respuesta en curso (o sale si no hay nada)

Ollama (cualquier subcomando del CLI oficial):
  /list  /ps  /show [m]  /run [m]  /stop [m]  /pull <m>  /push <m>
  /rm <m>  /cp <origen> <destino>  /create <nombre> -f <Modelfile>
  /signin  /signout  /launch  /version  /serve (inicia Ollama)
  /ollama <args>    cualquier otro comando: /ollama show qwen3:8b --modelfile
  /unload           libera de la RAM el modelo actual

Sesión:
  /use <modelo>     cambia de modelo           /clear      conversación nueva
  /tools /think /auto   activa o desactiva     /ctx <n>    tamaño de contexto
  /cd <carpeta>     carpeta de trabajo         /system <t> instrucciones extra
  /status           estado actual              /exit       salir
`)
}

func (c *cli) paint(code, s string) string {
	if !c.color || s == "" {
		return s
	}
	return code + s + cReset
}

func firstLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) <= n {
		return strings.Join(lines, "\n")
	}
	return strings.Join(lines[:n], "\n") + fmt.Sprintf("\n… (+%d líneas)", len(lines)-n)
}

func prefixLines(s, p string) string {
	return p + strings.ReplaceAll(s, "\n", "\n"+p)
}
