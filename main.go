package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/fsnotify/fsnotify"
)

var (
	clients   = make(map[chan struct{}]struct{})
	clientsMu sync.Mutex
)

// ------- Snippet que se inyecta en el HTML ---------

const liveReloadSnippet = `
<script>
(function() {
  try {
    var es = new EventSource('/events');
    es.onmessage = function(event) {
      if (event.data === 'reload') {
        console.log('[livereload] Change detected, reloading...');
        window.location.reload();
      }
    };
  } catch (e) {
    console.warn('[livereload] SSE not available', e);
  }
})();
</script>
`

func injectLiveReload(html []byte) []byte {
	lower := bytes.ToLower(html)
	idx := bytes.LastIndex(lower, []byte("</body>"))
	if idx == -1 {
		// No hay </body>, lo agregamos al final
		return append(html, []byte(liveReloadSnippet)...)
	}
	// Insertar justo antes de </body>
	var buf bytes.Buffer
	buf.Write(html[:idx])
	buf.WriteString(liveReloadSnippet)
	buf.Write(html[idx:])
	return buf.Bytes()
}

// ------- Live reload (SSE) ---------

func eventsHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming no soportado", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan struct{})
	clientsMu.Lock()
	clients[ch] = struct{}{}
	clientsMu.Unlock()

	defer func() {
		clientsMu.Lock()
		delete(clients, ch)
		clientsMu.Unlock()
		close(ch)
	}()

	notify := r.Context().Done()

	for {
		select {
		case <-ch:
			fmt.Fprintf(w, "data: reload\n\n")
			flusher.Flush()
		case <-notify:
			return
		}
	}
}

func livereloadJSHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript")
	script := `
(function() {
  var es = new EventSource('/events');
  es.onmessage = function(event) {
    if (event.data === 'reload') {
      console.log('[livereload] Change detected, reloading...');
      window.location.reload();
    }
  };
})();
`
	io.WriteString(w, script)
}

func broadcastReload() {
	clientsMu.Lock()
	defer clientsMu.Unlock()
	for ch := range clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// ------- File watcher ---------

func hasWatchedExt(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".php", ".html", ".css", ".js":
		return true
	default:
		return false
	}
}

func addWatchRecursive(w *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			log.Println("Error caminando directorio:", err)
			return nil
		}
		if d.IsDir() {
			if err := w.Add(path); err != nil {
				log.Println("No se pudo vigilar:", path, "error:", err)
			} else {
				log.Println("Vigilando:", path)
			}
		}
		return nil
	})
}

func startWatcher(root string) (*fsnotify.Watcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}

	if err := addWatchRecursive(watcher, root); err != nil {
		return nil, err
	}

	go func() {
		for {
			select {
			case ev, ok := <-watcher.Events:
				if !ok {
					return
				}
				// nuevos directorios
				if ev.Op&fsnotify.Create != 0 {
					info, err := os.Stat(ev.Name)
					if err == nil && info.IsDir() {
						_ = addWatchRecursive(watcher, ev.Name)
					}
				}
				// cambios relevantes
				if ev.Op&(fsnotify.Write|fsnotify.Create|fsnotify.Remove|fsnotify.Rename) != 0 {
					if hasWatchedExt(ev.Name) {
						log.Println("Cambio detectado en:", ev.Name)
						broadcastReload()
					}
				}
			case err, ok := <-watcher.Errors:
				if !ok {
					return
				}
				log.Println("Error watcher:", err)
			}
		}
	}()

	return watcher, nil
}

// ------- Servir PHP / estáticos con inyección ---------

func servePHP(w http.ResponseWriter, r *http.Request, scriptPath string) {
	// comprobar que el archivo existe
	if _, err := os.Stat(scriptPath); err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
		} else {
			http.Error(w, "Error interno", http.StatusInternalServerError)
		}
		return
	}

	cmd := exec.Command("php-cgi")

	env := os.Environ()
	env = append(env,
		"GATEWAY_INTERFACE=CGI/1.1",
		"SERVER_SOFTWARE=livephp/0.1",
		"SERVER_PROTOCOL="+r.Proto,
		"REQUEST_METHOD="+r.Method,
		"SCRIPT_FILENAME="+scriptPath,
		"SCRIPT_NAME="+r.URL.Path,
		"REQUEST_URI="+r.URL.RequestURI(),
		"QUERY_STRING="+r.URL.RawQuery,
		"REMOTE_ADDR="+r.RemoteAddr,
		"SERVER_NAME=localhost",
		"SERVER_PORT=9000",
		"REDIRECT_STATUS=200",
	)

	if ct := r.Header.Get("Content-Type"); ct != "" {
		env = append(env, "CONTENT_TYPE="+ct)
	}
	if r.ContentLength > 0 {
		env = append(env, "CONTENT_LENGTH="+strconv.FormatInt(r.ContentLength, 10))
	}

	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		http.Error(w, "No se pudo ejecutar PHP", http.StatusInternalServerError)
		return
	}

	go func() {
		if r.Body != nil {
			io.Copy(stdin, r.Body)
		}
		stdin.Close()
	}()

	out, err := cmd.CombinedOutput()
	if err != nil {
		log.Printf("Error ejecutando php-cgi: %v\nSalida completa:\n%s\n", err, string(out))
		http.Error(w, "Error ejecutando PHP", http.StatusInternalServerError)
		return
	}

	parts := bytes.SplitN(out, []byte("\r\n\r\n"), 2)
	if len(parts) != 2 {
		w.WriteHeader(http.StatusOK)
		w.Write(out)
		return
	}

	headerBlock := parts[0]
	body := parts[1]

	lines := bytes.Split(headerBlock, []byte("\r\n"))
	contentType := "text/html"

	for _, line := range lines {
		if len(line) == 0 {
			continue
		}
		colon := bytes.IndexByte(line, ':')
		if colon == -1 {
			continue
		}
		key := string(bytes.TrimSpace(line[:colon]))
		val := string(bytes.TrimSpace(line[colon+1:]))

		if strings.EqualFold(key, "Status") {
			fields := strings.Fields(val)
			if len(fields) > 0 {
				if code, err := strconv.Atoi(fields[0]); err == nil {
					w.WriteHeader(code)
				}
			}
		} else if strings.EqualFold(key, "Content-Type") {
			contentType = val
			w.Header().Add(key, val)
		} else {
			w.Header().Add(key, val)
		}
	}

	if strings.Contains(strings.ToLower(contentType), "text/html") {
		body = injectLiveReload(body)
	}

	w.Write(body)
}

func serveStaticWithInject(fullPath string, w http.ResponseWriter, r *http.Request) {
	ext := strings.ToLower(filepath.Ext(fullPath))
	if ext != ".html" && ext != ".htm" {
		// para CSS, JS, etc. servimos normal
		http.ServeFile(w, r, fullPath)
		return
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		http.Error(w, "No se pudo leer el archivo", http.StatusInternalServerError)
		return
	}

	data = injectLiveReload(data)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(data)
}

func serveFileOrPHP(root string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path

		if path == "" || path == "/" {
			if _, err := os.Stat(filepath.Join(root, "index.php")); err == nil {
				path = "/index.php"
			} else {
				path = "/index.html"
			}
		}

		cleanPath := filepath.Clean(path)
		fullPath := filepath.Join(root, cleanPath)

		if !strings.HasPrefix(fullPath, root) {
			http.Error(w, "Acceso no permitido", http.StatusForbidden)
			return
		}

		ext := strings.ToLower(filepath.Ext(fullPath))
		if ext == ".php" {
			servePHP(w, r, fullPath)
			return
		}

		serveStaticWithInject(fullPath, w, r)
	}
}

// ------- main ---------

func main() {
	dirFlag := flag.String("dir", ".", "Directorio raíz del proyecto (PHP/HTML/CSS/JS)")
	addrFlag := flag.String("addr", ":9000", "Dirección de escucha (ej: :9000)")
	flag.Parse()

	root, err := filepath.Abs(*dirFlag)
	if err != nil {
		log.Fatal("No se pudo obtener ruta absoluta:", err)
	}

	watcher, err := startWatcher(root)
	if err != nil {
		log.Fatal("No se pudo iniciar watcher:", err)
	}
	defer watcher.Close()

	http.HandleFunc("/events", eventsHandler)
	http.HandleFunc("/livereload.js", livereloadJSHandler) // opcional
	http.HandleFunc("/", serveFileOrPHP(root))

	log.Println("Sirviendo", root, "en", *addrFlag)
	log.Println("Abre: http://localhost" + strings.TrimPrefix(*addrFlag, ":"))
	log.Println("No necesitas modificar tus .php: el script de recarga se inyecta solo.")

	if err := http.ListenAndServe(*addrFlag, nil); err != nil {
		log.Fatal(err)
	}
}
