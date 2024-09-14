package main

import (
	"bytes"
	"errors"
	"flag"
	"html/template"
	"log"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"os"
	"path/filepath"
	"strings"

	"github.com/yuin/goldmark"
)

// Template for the rendered HTML pages.
// It includes placeholders for CSS links, the rendered content, and JS scripts.
const htmlTemplate = `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    {{- range .CSS }}
    <link rel="stylesheet" href="{{ . }}">
    {{- end }}
    <title>{{ .Title }}</title>
</head>
<body>
    {{ .Content }}
    {{- range .JS }}
    <script src="{{ . }}"></script>
    {{- end }}
</body>
</html>`

// PageData holds the data to be injected into the HTML template.
type PageData struct {
	Title   string
	CSS     []string
	JS      []string
	Content template.HTML
}

func main() {
	// Define command-line flags
	cssFlag := flag.String("css", "", "Comma-separated list of CSS source URLs to include")
	jsFlag := flag.String("js", "", "Comma-separated list of JS source URLs to include")

	// Parse the flags
	flag.Parse()

	// Ensure that basePath is provided as a positional argument
	if flag.NArg() < 1 {
		log.Fatalln("Usage: markdown_renderer [options] <base_path>")
	}

	basePath := flag.Arg(0)

	// Parse CSS and JS sources
	cssSources := parseSources(*cssFlag)
	jsSources := parseSources(*jsFlag)

	// Get absolute base path
	absBasePath, err := filepath.Abs(basePath)
	if err != nil {
		log.Fatalf("Error getting absolute base path: %v\n", err)
	}

	// Create the markdown handler with CSS and JS
	mdHandler, err := createMarkdownFSHandler(absBasePath, cssSources, jsSources)
	if err != nil {
		log.Fatalf("Error creating handler: %v\n", err)
	}

	// Register the handler
	http.Handle("/", mdHandler)

	// Start serving
	serve()
}

// parseSources splits a comma-separated string into a slice of strings, trimming spaces.
func parseSources(source string) []string {
	if source == "" {
		return nil
	}
	parts := strings.Split(source, ",")
	var sources []string
	for _, part := range parts {
		trimmed := strings.TrimSpace(part)
		if trimmed != "" {
			sources = append(sources, trimmed)
		}
	}
	return sources
}

// createMarkdownFSHandler creates an HTTP handler that serves files from basePath.
// If a requested file has a .md extension, it renders it as HTML with optional CSS and JS.
func createMarkdownFSHandler(basePath string, cssSources, jsSources []string) (http.Handler, error) {
	// Create the file server for static files
	fs := http.FileServer(http.Dir(basePath))

	// Parse the HTML template once
	tmpl, err := template.New("page").Parse(htmlTemplate)
	if err != nil {
		return nil, err
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Sanitize the requested path
		safePath, err := sanitizePath(basePath, filepath.Join(basePath, r.URL.Path))
		if err != nil {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		// Check if the path is a directory
		info, err := os.Stat(safePath)
		if err != nil {
			// If not found, serve as is (might result in 404)
			fs.ServeHTTP(w, r)
			return
		}

		if info.IsDir() {
			// Redirect directory to include trailing slash and index.md
			indexPath := strings.TrimSuffix(r.URL.Path, "/") + "/index.md"
			http.Redirect(w, r, indexPath, http.StatusMovedPermanently)
			return
		}

		if strings.HasSuffix(info.Name(), ".md") {
			// Serve the markdown file as rendered HTML
			renderMarkdown(w, safePath, tmpl, cssSources, jsSources)
			return
		}

		// For non-markdown files, serve them normally
		fs.ServeHTTP(w, r)
	}), nil
}

// renderMarkdown reads the markdown file, converts it to HTML, and writes the HTML response.
func renderMarkdown(w http.ResponseWriter, path string, tmpl *template.Template, cssSources, jsSources []string) {
	// Read the markdown file
	mdContent, err := os.ReadFile(path)
	if err != nil {
		http.Error(w, "Unable to read file", http.StatusInternalServerError)
		log.Printf("Error reading file %s: %v\n", path, err)
		return
	}

	// Convert markdown to HTML using Goldmark
	var buf bytes.Buffer
	if err := goldmark.Convert(mdContent, &buf); err != nil {
		http.Error(w, "Error rendering markdown", http.StatusInternalServerError)
		log.Printf("Error converting markdown %s: %v\n", path, err)
		return
	}

	// Prepare the data for the template
	data := PageData{
		Title:   extractTitle(mdContent),
		CSS:     cssSources,
		JS:      jsSources,
		Content: template.HTML(buf.String()),
	}

	// Execute the template
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tmpl.Execute(w, data); err != nil {
		http.Error(w, "Error rendering page", http.StatusInternalServerError)
		log.Printf("Error executing template for %s: %v\n", path, err)
		return
	}
}

// extractTitle extracts the first markdown header as the page title.
// If no header is found, it defaults to "Document".
func extractTitle(md []byte) string {
	lines := bytes.Split(md, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("# ")) {
			return string(bytes.TrimPrefix(line, []byte("# ")))
		}
	}
	return "Document"
}

// sanitizePath ensures that the requested path is within the base directory to prevent directory traversal.
func sanitizePath(absBasePath string, path string) (string, error) {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(absPath, absBasePath) {
		return "", errors.New("path outside allowed directory")
	}
	return absPath, nil
}

// serve attempts to serve via CGI first and falls back to an HTTP server if CGI fails.
func serve() {
	err := cgi.Serve(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pathInfo := os.Getenv("PATH_INFO")
		// Redirect to original path + "/" if there is no sub path
		if pathInfo == "" {
			redirectPath := os.Getenv("SCRIPT_NAME") + "/index.md"
			if r.URL.RawQuery != "" {
				redirectPath += "?" + r.URL.RawQuery
			}

			http.Redirect(w, r, redirectPath, http.StatusMovedPermanently)
			return
		}
		r.URL.Path = pathInfo
		http.DefaultServeMux.ServeHTTP(w, r)
	}))

	if err != nil {
		slog.Warn("Unable to serve via CGI, falling back to HTTP server", "error", err)
		port := ":8000"
		log.Printf("Serving HTTP on http://localhost%s\n", port)
		log.Fatal(http.ListenAndServe(port, nil))
	}
}
