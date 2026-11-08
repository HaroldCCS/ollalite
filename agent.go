package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strings"
	"time"
)

const maxRounds = 30 // máximo de rondas modelo→tools→modelo por mensaje

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	Images     []string   `json:"images,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolFunc struct {
	Index     int             `json:"index,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type ToolCall struct {
	ID       string   `json:"id,omitempty"`
	Function ToolFunc `json:"function"`
}

// ChatReq es lo que envía la interfaz (web o terminal).
type ChatReq struct {
	Model     string          `json:"model"`
	Messages  []Message       `json:"messages"`
	System    string          `json:"system"`
	Tools     bool            `json:"tools"`
	Auto      bool            `json:"auto"`
	Think     json.RawMessage `json:"think,omitempty"`
	Options   json.RawMessage `json:"options,omitempty"`
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`
	Cwd       string          `json:"cwd"`
}

type ollamaChat struct {
	Model     string          `json:"model"`
	Messages  []Message       `json:"messages"`
	Tools     json.RawMessage `json:"tools,omitempty"`
	Stream    bool            `json:"stream"`
	Think     json.RawMessage `json:"think,omitempty"`
	Options   json.RawMessage `json:"options,omitempty"`
	KeepAlive json.RawMessage `json:"keep_alive,omitempty"`
}

type chatChunk struct {
	Message struct {
		Content   string     `json:"content"`
		Thinking  string     `json:"thinking"`
		ToolCalls []ToolCall `json:"tool_calls"`
	} `json:"message"`
	Done            bool   `json:"done"`
	Error           string `json:"error"`
	TotalDuration   int64  `json:"total_duration"`
	LoadDuration    int64  `json:"load_duration"`
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	EvalDuration    int64  `json:"eval_duration"`
}

type Stats struct {
	Prompt  int   `json:"prompt"`
	Eval    int   `json:"eval"`
	EvalMs  int64 `json:"eval_ms"`
	TotalMs int64 `json:"total_ms"`
	LoadMs  int64 `json:"load_ms"`
}

// Event es cada línea del stream hacia la interfaz.
//
//	think/tok: texto incremental · call: el modelo pide una tool (need = requiere aprobación)
//	result: salida de la tool (M = mensaje role=tool) · msg: mensaje del asistente terminado
//	stats: métricas · err: error · done: fin
type Event struct {
	T    string          `json:"t"`
	D    string          `json:"d,omitempty"`
	ID   string          `json:"id,omitempty"`
	Name string          `json:"name,omitempty"`
	Args json.RawMessage `json:"args,omitempty"`
	Need bool            `json:"need,omitempty"`
	Err  bool            `json:"err,omitempty"`
	M    *Message        `json:"m,omitempty"`
	S    *Stats          `json:"s,omitempty"`
	// Solo en eventos "st" (estado en vivo): fase, ms en la fase, fragmentos recibidos, tokens estimados del prompt.
	P   string `json:"p,omitempty"`
	Ms  int64  `json:"ms,omitempty"`
	N   int    `json:"n,omitempty"`
	Est int    `json:"est,omitempty"`
}

type approver func(ctx context.Context, id, name string, args json.RawMessage) bool

// runAgent: bucle modelo → tools → modelo hasta que el modelo responde sin pedir tools.
// st registra en qué fase está (lo usa el latido de la interfaz web).
func runAgent(ctx context.Context, req *ChatReq, emit func(Event), approve approver, st *Status) error {
	if req.Model == "" {
		return errors.New("elige un modelo primero")
	}
	cwd := expandPath(req.Cwd, defaultCwd)
	tc := &toolCtx{cwd: cwd}
	msgs := make([]Message, 0, len(req.Messages)+8)
	if len(req.Messages) == 0 || req.Messages[0].Role != "system" {
		msgs = append(msgs, Message{Role: "system", Content: systemPrompt(req.System, cwd, req.Tools)})
	}
	msgs = append(msgs, req.Messages...)

	body := ollamaChat{Model: req.Model, Stream: true, Think: req.Think, Options: req.Options, KeepAlive: req.KeepAlive}
	if req.Tools {
		body.Tools = toolSchemas
	}
	for round := 0; round < maxRounds; round++ {
		body.Messages = msgs
		payload, err := json.Marshal(&body)
		if err != nil {
			return err
		}
		st.newRound(len(payload) / 4) // ~4 caracteres por token
		emit(st.snapshot())
		asst, err := streamChat(ctx, payload, emit, st)
		if err != nil {
			return err
		}
		emit(Event{T: "msg", M: &asst})
		msgs = append(msgs, asst)
		if len(asst.ToolCalls) == 0 {
			return nil
		}
		for i, call := range asst.ToolCalls {
			id := fmt.Sprintf("%s-%d-%d", randHex(3), round, i)
			name, args := call.Function.Name, call.Function.Arguments
			need := !req.Auto && needsApproval(name)
			call := Event{T: "call", ID: id, Name: name, Args: args, Need: need}
			emit(call)
			denied := false
			if need {
				st.waitApproval(&call) // el latido repite la petición: la tarjeta no se puede perder
				emit(st.snapshot())
				if !approve(ctx, id, name, args) {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					denied = true
				}
			}
			var out string
			var failed bool
			if denied {
				out, failed = "The user rejected this action. Do not retry it; ask the user what to do instead.", true
			} else {
				st.set("tool", name+" · "+argSummary(name, args))
				emit(st.snapshot())
				out, failed = runTool(ctx, tc, name, args)
			}
			tm := Message{Role: "tool", Content: out, ToolName: name, ToolCallID: call.ID}
			emit(Event{T: "result", ID: id, Name: name, Err: failed, M: &tm})
			msgs = append(msgs, tm)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("se alcanzó el límite de %d rondas de herramientas", maxRounds)
}

// streamChat llama a /api/chat de Ollama en streaming y reenvía tokens a la interfaz.
func streamChat(ctx context.Context, payload []byte, emit func(Event), st *Status) (Message, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", ollamaBase.String()+"/api/chat", bytes.NewReader(payload))
	if err != nil {
		return Message{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return Message{}, ctx.Err()
		}
		return Message{}, fmt.Errorf("no se pudo conectar con Ollama en %s (¿está abierto?)", ollamaBase)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(data, &e)
		if e.Error == "" {
			e.Error = strings.TrimSpace(string(data))
		}
		return Message{}, fmt.Errorf("Ollama: %s", e.Error)
	}

	msg := Message{Role: "assistant"}
	var text strings.Builder
	visible, evalCount := 0, 0
	rd := bufio.NewReaderSize(resp.Body, 32<<10)
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var c chatChunk
			if json.Unmarshal(line, &c) == nil {
				if c.Error != "" {
					return msg, errors.New("Ollama: " + c.Error)
				}
				if c.Message.Thinking != "" {
					visible++
					st.chunk("think")
					emit(Event{T: "think", D: c.Message.Thinking})
				}
				if c.Message.Content != "" {
					visible++
					st.chunk("write")
					text.WriteString(c.Message.Content)
					emit(Event{T: "tok", D: c.Message.Content})
				}
				for _, tc := range c.Message.ToolCalls {
					tc.Function.Arguments = normArgs(tc.Function.Arguments)
					msg.ToolCalls = append(msg.ToolCalls, tc)
				}
				if c.Done {
					evalCount = c.EvalCount
					emit(Event{T: "stats", S: &Stats{
						Prompt: c.PromptEvalCount, Eval: c.EvalCount,
						EvalMs: c.EvalDuration / 1e6, TotalMs: c.TotalDuration / 1e6, LoadMs: c.LoadDuration / 1e6,
					}})
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return msg, rerr
		}
	}
	// Ollama oculta los tokens de una llamada a herramienta mientras la genera. Si al final no
	// logró interpretarla, la descarta en silencio: avisamos para que no parezca que se hizo algo.
	if hidden := evalCount - visible; len(msg.ToolCalls) == 0 && hidden > 60 && hidden > evalCount/4 {
		emit(Event{T: "warn", D: fmt.Sprintf("El modelo generó %d tokens que no se mostraron: intentó usar una herramienta (por ejemplo, escribir un archivo), pero Ollama no pudo interpretar la llamada y la descartó. No se ejecutó nada. Pídele que lo intente de nuevo o prueba con otro modelo.", hidden)})
	}
	msg.Content = text.String()
	return msg, nil
}

// normArgs garantiza que los argumentos sean un objeto JSON (algunos modelos los mandan como string).
func normArgs(a json.RawMessage) json.RawMessage {
	a = bytes.TrimSpace(a)
	if len(a) == 0 || string(a) == "null" {
		return json.RawMessage("{}")
	}
	if a[0] == '"' {
		var s string
		if json.Unmarshal(a, &s) == nil && json.Valid([]byte(s)) {
			return json.RawMessage(s)
		}
	}
	return a
}

func systemPrompt(extra, cwd string, tools bool) string {
	var b strings.Builder
	b.WriteString("You are a helpful assistant running locally on the user's computer through Ollama. Always answer in the same language the user writes in. Be concise and practical.\n")
	fmt.Fprintf(&b, "\nEnvironment: OS=%s/%s, shell=%s, home=%s, working directory=%s, now=%s.\n",
		runtime.GOOS, runtime.GOARCH, shellName(), homeDir, cwd, time.Now().Format("2006-01-02 15:04 (Monday)"))
	if tools {
		b.WriteString(`
You can act on the user's computer with these tools: read_file, write_file, edit_file, list_dir, find_files, search_text, run_command, fetch_url.
- Use them whenever the request involves files, folders, programs, the system or the web. Never invent file contents or command output: call a tool and use the real result.
- Relative paths resolve against the working directory; ~ is the home folder.
- To locate something "in my folders/computer", use find_files (by file name, or by words inside documents with "content") starting at the home folder; use search_text to grep inside code/text files.
- Read a file before editing it. Use edit_file for targeted changes and write_file for new files or full rewrites.
- run_command is non-interactive: pass flags like -y when needed. Avoid destructive commands unless the user clearly asked for them.
- When later steps depend on earlier results, call one tool at a time. When finished, briefly tell the user what you did.
- Never claim that you created, changed, ran or found something unless a tool call in this conversation actually did it and returned success. If you have not called the tool yet, call it now instead of describing the result.
`)
		if runtime.GOOS == "darwin" {
			b.WriteString("- macOS standard folders always have English paths even if the user names them in Spanish: Escritorio = ~/Desktop, Documentos = ~/Documents, Descargas = ~/Downloads, Imágenes = ~/Pictures, Música = ~/Music.\n")
		}
	}
	if s := strings.TrimSpace(extra); s != "" {
		b.WriteString("\nAdditional instructions from the user:\n" + s + "\n")
	}
	return b.String()
}
