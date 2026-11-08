#!/bin/sh
# Compila OllaLite: un único binario sin dependencias.
#   ./build.sh        → ./ollalite
#   ./build.sh app    → además OllaLite.app (macOS, doble clic para abrir)
set -e
cd "$(dirname "$0")"
go build -trimpath -ldflags="-s -w" -o ollalite .
echo "✓ ./ollalite ($(du -h ollalite | cut -f1 | tr -d ' '))"

if [ "$1" = "app" ] && [ "$(uname)" = "Darwin" ]; then
  APP=OllaLite.app
  rm -rf "$APP"
  mkdir -p "$APP/Contents/MacOS"
  cp ollalite "$APP/Contents/MacOS/ollalite-bin"  # nombre distinto: el disco de macOS no distingue mayúsculas
  cat > "$APP/Contents/MacOS/OllaLite" <<'SH'
#!/bin/sh
# Abre la interfaz como ventana de app (Chrome/Edge/Brave) o en el navegador por defecto.
exec "$(dirname "$0")/ollalite-bin" -app
SH
  chmod +x "$APP/Contents/MacOS/OllaLite"
  cat > "$APP/Contents/Info.plist" <<'PL'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>CFBundleName</key><string>OllaLite</string>
  <key>CFBundleDisplayName</key><string>OllaLite</string>
  <key>CFBundleIdentifier</key><string>local.ollalite</string>
  <key>CFBundleExecutable</key><string>OllaLite</string>
  <key>CFBundlePackageType</key><string>APPL</string>
  <key>CFBundleShortVersionString</key><string>1.0.0</string>
  <key>LSUIElement</key><true/>
</dict></plist>
PL
  codesign --force --deep -s - "$APP" >/dev/null 2>&1 || true
  echo "✓ $APP  (arrástrala a /Applications; ciérrala con el botón Salir de la interfaz)"
fi
