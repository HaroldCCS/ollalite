package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func tool(t *testing.T, tc *toolCtx, name, args string) (string, bool) {
	t.Helper()
	return runTool(context.Background(), tc, name, json.RawMessage(args))
}

func TestFileTools(t *testing.T) {
	dir := t.TempDir()
	homeDir = dir
	tc := &toolCtx{cwd: dir}

	if out, bad := tool(t, tc, "write_file", `{"path":"sub/a.txt","content":"uno\ndos\ntres\n"}`); bad || !strings.Contains(out, "Created") {
		t.Fatalf("write_file: %s", out)
	}
	if out, bad := tool(t, tc, "read_file", `{"path":"sub/a.txt","offset":"2","limit":1}`); bad || !strings.Contains(out, "2\tdos") || strings.Contains(out, "tres\n") {
		t.Fatalf("read_file con offset/limit como string: %s", out)
	}
	if out, bad := tool(t, tc, "edit_file", `{"path":"sub/a.txt","old_text":"dos","new_text":"DOS"}`); bad {
		t.Fatalf("edit_file: %s", out)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub/a.txt")); string(b) != "uno\nDOS\ntres\n" {
		t.Fatalf("contenido tras editar: %q", b)
	}
	if out, bad := tool(t, tc, "edit_file", `{"path":"sub/a.txt","old_text":"no existe","new_text":"x"}`); !bad || !strings.Contains(out, "not found") {
		t.Fatalf("edit_file debía fallar: %s", out)
	}
	// argumentos como string JSON (algunos modelos lo hacen)
	if out, bad := tool(t, tc, "list_dir", `"{\"path\":\"sub\"}"`); bad || !strings.Contains(out, "a.txt") {
		t.Fatalf("list_dir: %s", out)
	}
	if out, bad := tool(t, tc, "search_text", `{"pattern":"DOS|tres","path":"."}`); bad || !strings.Contains(out, "a.txt:2: DOS") || !strings.Contains(out, "a.txt:3: tres") {
		t.Fatalf("search_text: %s", out)
	}
	if out, bad := tool(t, tc, "search_text", `{"pattern":"f(x","path":"."}`); bad || !strings.Contains(out, "No matches") {
		t.Fatalf("search_text con regex inválida debe buscar literal: %s", out)
	}
	if out, bad := tool(t, tc, "find_files", `{"pattern":"*.txt","path":"."}`); bad || !strings.Contains(out, "a.txt") {
		t.Fatalf("find_files glob: %s", out)
	}
	if out, bad := tool(t, tc, "find_files", `{"pattern":"zzz-no-existe","path":"."}`); bad || !strings.Contains(out, "No files") {
		t.Fatalf("find_files sin resultados: %s", out)
	}
}

func TestRunCommand(t *testing.T) {
	tc := &toolCtx{cwd: t.TempDir()}
	if out, bad := tool(t, tc, "run_command", `{"command":"echo hola; exit 3"}`); !bad || !strings.Contains(out, "exit code 3") || !strings.Contains(out, "hola") {
		t.Fatalf("exit code: %s", out)
	}
	start := time.Now()
	if out, bad := tool(t, tc, "run_command", `{"command":"sleep 30 & sleep 30","timeout_sec":1}`); !bad || !strings.Contains(out, "timeout") {
		t.Fatalf("timeout: %s", out)
	}
	if time.Since(start) > 6*time.Second {
		t.Fatal("el timeout no mató el árbol de procesos a tiempo")
	}
	out, _ := tool(t, tc, "run_command", `{"command":"seq 1 200000"}`)
	if len(out) > capHead+capTail+200 || !strings.Contains(out, "bytes omitted") || !strings.HasSuffix(out, "200000") {
		t.Fatalf("salida no acotada (%d bytes)", len(out))
	}
}

func TestSplitArgs(t *testing.T) {
	got := splitArgs(`run qwen3:8b "hola mundo" 'a b'`)
	want := []string{"run", "qwen3:8b", "hola mundo", "a b"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", got)
	}
}

// OLLALITE_HOME_SEARCH=1 go test -run TestHomeSearch -v  → mide la búsqueda en todo el home.
func TestHomeSearch(t *testing.T) {
	if os.Getenv("OLLALITE_HOME_SEARCH") == "" {
		t.Skip("solo bajo demanda")
	}
	homeDir, _ = os.UserHomeDir()
	tc := &toolCtx{cwd: homeDir}
	for _, args := range []string{`{"pattern":"*.pdf","max_results":5}`, `{"pattern":"matematicas","max_results":5}`, `{"content":"derivada","max_results":5}`} {
		start := time.Now()
		out, _ := tool(t, tc, "find_files", args)
		t.Logf("%s → %v\n%s", args, time.Since(start).Round(time.Millisecond), out)
	}
}
