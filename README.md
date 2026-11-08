# 🍲 OllaLite

Interfaz local ultraligera para [Ollama](https://ollama.com): chat con herramientas (archivos, comandos, búsqueda en tus carpetas, web) y todos los comandos de Ollama, en **un solo binario de ~6,5 MB que usa ~10-14 MB de RAM**. El resto de la memoria queda para el modelo.

## Instalación en 5 pasos

### 1. Instala Ollama

- **macOS y Windows:** descárgalo de [ollama.com/download](https://ollama.com/download), instálalo y ábrelo. En macOS queda un icono de llama en la barra de menú.
- **Linux:**
  ```bash
  curl -fsSL https://ollama.com/install.sh | sh
  ```

Comprueba que funciona:
```bash
ollama --version
```

### 2. Descarga y prueba tu primer modelo

Elige uno según la RAM de tu equipo. Todos estos saben usar herramientas (tools):

| Tu RAM | Modelo | Tamaño |
|---|---|---|
| 8 GB | `qwen3:4b` | ~2,5 GB |
| 16 GB | `qwen3:8b` (recomendado) | ~5,2 GB |
| 32 GB o más | `qwen3:14b` | ~9,3 GB |

```bash
ollama pull qwen3:8b
```
```bash
ollama run qwen3:8b "Hola, preséntate en una frase"
```

Si responde, Ollama está listo. También puedes descargar modelos después desde la pestaña **Modelos** de OllaLite.

### 3. Instala Go (solo hace falta para compilar)

- **macOS:** `brew install go`, o el instalador de [go.dev/dl](https://go.dev/dl)
- **Windows y Linux:** el instalador de [go.dev/dl](https://go.dev/dl)

Necesitas la versión 1.22 o superior:
```bash
go version
```

### 4. Descarga y compila OllaLite

```bash
git clone https://github.com/HaroldCCS/ollalite.git
cd ollalite
./build.sh
```

En Windows (PowerShell), en lugar de `./build.sh`:
```powershell
go build -o ollalite.exe .
```

### 5. Ábrelo

```bash
./ollalite
```

Se abre el navegador en http://127.0.0.1:11500. Elige el modelo arriba a la izquierda y escribe. En Windows usa `.\ollalite.exe`.

- Si aparece **"Ollama no responde"**, abre la app de Ollama o pulsa el botón **Iniciar Ollama**.
- Para cerrar OllaLite, pulsa **Salir** en la barra lateral (o Ctrl+C en la terminal).
- ¿Prefieres la terminal? `./ollalite -cli`
- ¿Quieres una app con doble clic en macOS? `./build.sh app` crea `OllaLite.app`.

## ¿Dónde se guardan mis conversaciones?

Nunca dentro de la carpeta del proyecto, así que no se suben a GitHub:

- **Interfaz web:** en tu navegador (almacenamiento local del sitio `127.0.0.1:11500`). Para borrarlas: **Ajustes → Borrar todos los chats**.
- **Modo terminal (`-cli`):** solo en memoria; se pierden al salir.
- **`ollama run`** (el comando de Ollama, no OllaLite): guarda lo que escribes en `~/.ollama/history`.

## Cómo se logra el bajo consumo

| Pieza | Decisión |
|---|---|
| Backend | Go, solo librería estándar (0 dependencias), binario estático |
| Interfaz | Un único HTML con JS puro (sin React, sin fuentes externas, sin CDN), embebido en el binario |
| Memoria | `GOGC=50` + techo blando de 32 MB + devolución de memoria al SO tras cada respuesta |
| Streaming | Los tokens pasan directo de Ollama a la pantalla, sin guardar copias grandes |
| Salidas | Comandos y lecturas de archivos acotados (inicio + final) para no llenar RAM ni contexto |
| Modo terminal | `-cli`: sin navegador ni servidor HTTP, la opción de menor consumo |

Medido en este Mac (M-series, 16 GB): web en reposo **10,5 MB**, pico durante chat + búsqueda en todo el home **13,5 MB**; modo terminal **~12 MB**.

## Uso

```bash
./build.sh            # compila ./ollalite (necesita Go 1.22+)
./ollalite            # abre la interfaz en el navegador → http://127.0.0.1:11500
./ollalite -app       # la abre como ventana de app (Chrome/Edge/Brave)
./ollalite -cli       # modo terminal, sin navegador
./build.sh app        # además crea OllaLite.app (macOS, doble clic)
```

Opciones: `-port 11500` · `-dir ~/proyectos` (carpeta de trabajo) · `-ollama host:puerto` (o `OLLAMA_HOST`) · `-model qwen3:8b` y `-auto` (modo terminal) · `-no-open`.

## Qué incluye

**Estado en vivo**: debajo del chat una barra dice qué está pasando y cuánto lleva: cargando el modelo en RAM, leyendo el mensaje (~tokens), razonando, generando sin texto visible (cuando el modelo escribe el contenido de una herramienta, que Ollama no muestra hasta terminar), ejecutando una tool o esperando tu permiso. El servidor manda un latido cada segundo y vigila a Ollama: si Ollama o el modelo se caen, o se pierde la conexión con la app, lo verás en rojo en lugar de puntos girando para siempre. Si el modelo intenta usar una herramienta y Ollama descarta la llamada, aparece un aviso. Con **Pensar** activado, el razonamiento se ve en vivo.

**Chat**: streaming, markdown con bloques de código copiables, razonamiento (modelos *thinking*), imágenes (modelos con visión; también pegando con Cmd+V), métricas tok/s, historial de chats guardado en el navegador.

**Tools por defecto** (para modelos con soporte de tools: qwen3, qwen2.5, llama3.1+, etc.):

| Tool | Qué hace | ¿Pide permiso? |
|---|---|---|
| `read_file` | Lee archivos de texto con números de línea y paginación | No |
| `write_file` | Crea o reescribe archivos (crea carpetas) | **Sí** |
| `edit_file` | Reemplazo exacto de texto, con vista de diferencias | **Sí** |
| `list_dir` | Lista carpetas (hasta 4 niveles) | No |
| `find_files` | Busca por nombre (`*.pdf`, `tesis`) **o por contenido dentro de PDF/Word** en todo tu home. En macOS usa Spotlight (~1 s) e incluye iCloud Drive | No |
| `search_text` | grep paralelo con regex dentro de código/texto | No |
| `run_command` | Ejecuta comandos de shell con timeout y mata el árbol de procesos | **Sí** |
| `fetch_url` | Descarga una página web como texto | No |

Las acciones que modifican tu equipo muestran una tarjeta **Permitir / Rechazar / Permitir siempre**. El interruptor **Auto** las aprueba todas.

**Modelos** (equivalentes a los comandos de Ollama): `list` con capacidades, `pull`/`push` con barra de progreso y velocidad, `cp`, `rm`, `show` (parámetros, template, licencia), `ps` con RAM/GPU/contexto, `stop` (liberar RAM), precarga, y `create` desde un Modelfile.

**Consola**: ejecuta cualquier subcomando del CLI oficial (`run`, `signin`, `launch`, flags como `--verbose`…) con salida en vivo.

**Ajustes**: carpeta de trabajo, instrucciones de sistema extra, `num_ctx`, temperatura y `keep_alive` (`0` libera la RAM al terminar cada respuesta, `-1` deja el modelo siempre cargado).

## Modo terminal

```
› ¿qué archivos grandes tengo en Descargas?
⚙ run_command du -sh ~/Downloads/* | sort -h | tail
  ¿Permitir? [s]í / [n]o / [t]odo:
```

`/help` lista todo: `/list /ps /pull /rm /cp /show /run /stop /create /signin…` (cualquier comando de Ollama), `/use <modelo>`, `/unload`, `/tools /think /auto`, `/cd`, `/ctx`, `!comando` para shell directo y bloques multilínea con `"""`.

## Seguridad

- Solo escucha en `127.0.0.1`.
- Cada arranque genera un token secreto que solo conoce la página servida; además se valida `Host` y `Origin`. Así ninguna web que tengas abierta puede usar la API local para ejecutar comandos (protege contra CSRF y DNS rebinding).
- La página no se puede embeber en iframes (anti-clickjacking) y el markdown del modelo se escapa siempre.
- Escritura y comandos piden permiso salvo que actives **Auto**.

## Estructura

```
main.go        servidor HTTP, proxy a la API de Ollama, consola, seguridad
agent.go       bucle modelo → tools → modelo (streaming NDJSON)
tools.go       las 8 tools
cli.go         modo terminal
proc_*.go      procesos por sistema operativo (macOS/Linux y Windows)
ui/index.html  interfaz completa (HTML + CSS + JS puro)
```

`go test ./...` ejecuta las pruebas de las tools.
