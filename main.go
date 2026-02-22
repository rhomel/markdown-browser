package main

import (
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

type treeEntry struct {
	Name      string
	RelPath   string
	IsDir     bool
	IsIndexMD bool
	Children  []treeEntry
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: %s <server|generate> [args]", os.Args[0])
	}

	switch os.Args[1] {
	case "server":
		runServer(os.Args[2:])
	case "generate":
		runGenerate(os.Args[2:])
	default:
		fatalf("unknown command %q", os.Args[1])
	}
}

func runServer(args []string) {
	fsFlags := flag.NewFlagSet("server", flag.ExitOnError)
	listenAddr := fsFlags.String("listen", "0.0.0.0:3333", "listen interface/address")
	fsFlags.Parse(args)

	if fsFlags.NArg() != 1 {
		fatalf("usage: %s server [-listen=0.0.0.0:3333] <markdown-dir>", os.Args[0])
	}

	rootArg := fsFlags.Arg(0)
	rootAbs, err := filepath.Abs(rootArg)
	if err != nil {
		fatalf("resolve root dir: %v", err)
	}
	if err := ensureDir(rootAbs); err != nil {
		fatalf("invalid markdown dir: %v", err)
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s", r.Method, r.URL.Path)
		handleRequest(w, r, rootAbs)
	})

	serverURL := "http://" + *listenAddr
	log.Printf("listening on %s", serverURL)
	if err := http.ListenAndServe(*listenAddr, handler); err != nil {
		fatalf("http server error: %v", err)
	}
}

func handleRequest(w http.ResponseWriter, r *http.Request, rootAbs string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cleanPath := path.Clean("/" + r.URL.Path)
	if cleanPath == "." {
		cleanPath = "/"
	}

	rel, err := reqToRel(cleanPath)
	if err != nil {
		notFound(w)
		return
	}

	if rel == "." {
		serveDirIndex(w, rootAbs, "")
		return
	}

	abs := filepath.Join(rootAbs, rel)
	if !isWithinRoot(rootAbs, abs) {
		notFound(w)
		return
	}
	if isIgnoredRelPath(rel) {
		notFound(w)
		return
	}

	st, err := os.Stat(abs)
	if err == nil && st.IsDir() {
		if !isReadablePath(abs) {
			notFound(w)
			return
		}
		serveDirIndex(w, rootAbs, rel)
		return
	}

	if strings.HasSuffix(rel, ".md") {
		serveMarkdownSource(w, rootAbs, rel)
		return
	}

	if strings.HasSuffix(rel, ".html") {
		base := strings.TrimSuffix(rel, ".html")
		mdRel := base + ".md"
		serveRenderedMarkdown(w, rootAbs, mdRel)
		return
	}

	notFound(w)
}

func runGenerate(args []string) {
	fsFlags := flag.NewFlagSet("generate", flag.ExitOnError)
	outDir := fsFlags.String("out", "", "output directory (required)")
	fsFlags.Parse(args)

	if *outDir == "" {
		fatalf("generate requires -out")
	}
	if fsFlags.NArg() != 1 {
		fatalf("usage: %s generate -out <output-dir> <markdown-dir>", os.Args[0])
	}

	rootArg := fsFlags.Arg(0)
	rootAbs, err := filepath.Abs(rootArg)
	if err != nil {
		fatalf("resolve markdown dir: %v", err)
	}
	if err := ensureDir(rootAbs); err != nil {
		fatalf("invalid markdown dir: %v", err)
	}

	outAbs, err := filepath.Abs(*outDir)
	if err != nil {
		fatalf("resolve output dir: %v", err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		fatalf("resolve current directory: %v", err)
	}
	cwdAbs, err := filepath.Abs(cwd)
	if err != nil {
		fatalf("resolve current directory abs path: %v", err)
	}
	if sameDir(outAbs, cwdAbs) {
		fatalf("output directory cannot be the same as the current directory")
	}
	if sameDir(rootAbs, outAbs) {
		fatalf("output directory cannot be the same as markdown directory")
	}

	if err := os.MkdirAll(outAbs, 0o755); err != nil {
		fatalf("create output dir: %v", err)
	}

	if err := generateAll(rootAbs, outAbs); err != nil {
		fatalf("generate failed: %v", err)
	}
}

func generateAll(rootAbs, outAbs string) error {
	hasRootIndexMD, err := dirHasIndexMD(rootAbs)
	if err != nil {
		return err
	}
	if !hasRootIndexMD {
		data, err := renderDirectoryHTML(rootAbs, "")
		if err != nil {
			return err
		}
		outFile := filepath.Join(outAbs, "index.html")
		if err := os.WriteFile(outFile, []byte(data), 0o644); err != nil {
			return err
		}
		fmt.Println(outFile)
	}

	return filepath.WalkDir(rootAbs, func(pathAbs string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsPermission(err) {
				if d != nil && d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			return err
		}

		rel, err := filepath.Rel(rootAbs, pathAbs)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if isIgnoredBaseName(d.Name()) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !isReadablePath(pathAbs) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}

		if d.IsDir() {
			outDirPath := filepath.Join(outAbs, rel)
			if err := os.MkdirAll(outDirPath, 0o755); err != nil {
				return err
			}

			hasIndexMD, err := dirHasIndexMD(pathAbs)
			if err != nil {
				return err
			}
			if hasIndexMD {
				return nil
			}

			data, err := renderDirectoryHTML(rootAbs, rel)
			if err != nil {
				return err
			}
			outFile := filepath.Join(outDirPath, "index.html")
			if err := os.WriteFile(outFile, []byte(data), 0o644); err != nil {
				return err
			}
			fmt.Println(outFile)
			return nil
		}

		if strings.EqualFold(filepath.Base(rel), "index.md") {
			outFile := filepath.Join(outAbs, filepath.Dir(rel), "index.html")
			return renderMDFileTo(rootAbs, rel, outFile)
		}

		if strings.HasSuffix(rel, ".md") {
			outFile := filepath.Join(outAbs, strings.TrimSuffix(rel, ".md")+".html")
			return renderMDFileTo(rootAbs, rel, outFile)
		}

		return nil
	})
}

func renderMDFileTo(rootAbs, mdRel, outFile string) error {
	pageHTML, err := renderMarkdownPage(rootAbs, mdRel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outFile), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(outFile, []byte(pageHTML), 0o644); err != nil {
		return err
	}
	fmt.Println(outFile)
	return nil
}

func serveDirIndex(w http.ResponseWriter, rootAbs, relDir string) {
	data, err := renderDirectoryHTML(rootAbs, relDir)
	if err != nil {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeHTTPString(w, data)
}

func serveMarkdownSource(w http.ResponseWriter, rootAbs, mdRel string) {
	abs := filepath.Join(rootAbs, filepath.FromSlash(mdRel))
	if !isWithinRoot(rootAbs, abs) {
		notFound(w)
		return
	}
	if isIgnoredRelPath(mdRel) {
		notFound(w)
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	writeHTTPBytes(w, data)
}

func serveRenderedMarkdown(w http.ResponseWriter, rootAbs, mdRel string) {
	pageHTML, err := renderMarkdownPage(rootAbs, mdRel)
	if err != nil {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeHTTPString(w, pageHTML)
}

func renderDirectoryHTML(rootAbs, relDir string) (string, error) {
	dirAbs := filepath.Join(rootAbs, filepath.FromSlash(relDir))
	if err := ensureDir(dirAbs); err != nil {
		return "", err
	}
	if relDir != "" && isIgnoredRelPath(relDir) {
		return "", errors.New("ignored path")
	}
	if !isReadablePath(dirAbs) {
		return "", errors.New("directory not readable")
	}

	entries, err := buildTree(rootAbs, relDir)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\"><title>Index</title></head><body>")
	renderTreeHTML(&b, entries, relDir)
	b.WriteString("</body></html>")
	return b.String(), nil
}

func buildTree(rootAbs, relDir string) ([]treeEntry, error) {
	dirAbs := filepath.Join(rootAbs, filepath.FromSlash(relDir))
	list, err := os.ReadDir(dirAbs)
	if err != nil {
		return nil, err
	}

	var out []treeEntry
	for _, e := range list {
		name := e.Name()
		if isIgnoredBaseName(name) {
			continue
		}
		relChild := filepath.ToSlash(filepath.Join(relDir, name))
		childAbs := filepath.Join(rootAbs, filepath.FromSlash(relChild))
		if e.IsDir() {
			if !isReadablePath(childAbs) {
				continue
			}
			children, err := buildTree(rootAbs, relChild)
			if err != nil {
				if os.IsPermission(err) {
					continue
				}
				return nil, err
			}
			out = append(out, treeEntry{
				Name:     name,
				RelPath:  relChild,
				IsDir:    true,
				Children: children,
			})
			continue
		}

		if !strings.HasSuffix(name, ".md") {
			continue
		}
		if !isReadablePath(childAbs) {
			continue
		}

		base := strings.TrimSuffix(name, ".md")
		isIndex := strings.EqualFold(name, "index.md")
		out = append(out, treeEntry{
			Name:      base,
			RelPath:   filepath.ToSlash(filepath.Join(relDir, base+".html")),
			IsIndexMD: isIndex,
		})
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	})

	return out, nil
}

func renderTreeHTML(b *strings.Builder, entries []treeEntry, relDir string) {
	b.WriteString("<ul>")
	for _, e := range entries {
		if e.IsIndexMD {
			continue
		}
		b.WriteString("<li>")
		if e.IsDir {
			href := "/" + strings.TrimPrefix(e.RelPath, "./")
			b.WriteString(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(e.Name) + `</a>`)
			renderTreeHTML(b, e.Children, e.RelPath)
		} else {
			href := "/" + strings.TrimPrefix(e.RelPath, "./")
			b.WriteString(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(e.Name) + `</a>`)
		}
		b.WriteString("</li>")
	}
	b.WriteString("</ul>")
}

func renderMarkdownFile(rootAbs, mdRel string) (string, error) {
	mdRel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(mdRel)))
	abs := filepath.Join(rootAbs, filepath.FromSlash(mdRel))
	if !isWithinRoot(rootAbs, abs) {
		return "", errors.New("path escape")
	}
	if isIgnoredRelPath(mdRel) {
		return "", errors.New("ignored path")
	}
	if !isReadablePath(abs) {
		return "", errors.New("file not readable")
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", err
	}

	var out strings.Builder
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
	)
	if err := md.Convert(data, &out); err != nil {
		return "", err
	}
	return out.String(), nil
}

func renderMarkdownPage(rootAbs, mdRel string) (string, error) {
	htmlBody, err := renderMarkdownFile(rootAbs, mdRel)
	if err != nil {
		return "", err
	}
	return wrapHTMLPage(htmlBody), nil
}

func wrapHTMLPage(body string) string {
	return "<!doctype html><html><head><meta charset=\"utf-8\"></head><body>" + body + "</body></html>"
}

func reqToRel(reqPath string) (string, error) {
	trimmed := strings.TrimPrefix(reqPath, "/")
	if trimmed == "" {
		return ".", nil
	}
	clean := filepath.Clean(filepath.FromSlash(trimmed))
	if clean == "." || strings.HasPrefix(clean, "..") {
		return "", errors.New("invalid path")
	}
	return filepath.ToSlash(clean), nil
}

func isWithinRoot(rootAbs, pathAbs string) bool {
	rootClean := filepath.Clean(rootAbs)
	pathClean := filepath.Clean(pathAbs)
	rel, err := filepath.Rel(rootClean, pathClean)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && rel != "")
}

func ensureDir(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("not a directory")
	}
	return nil
}

func sameDir(a, b string) bool {
	aEval, errA := filepath.EvalSymlinks(a)
	if errA != nil {
		aEval = filepath.Clean(a)
	}
	bEval, errB := filepath.EvalSymlinks(b)
	if errB != nil {
		bEval = filepath.Clean(b)
	}
	return aEval == bEval
}

func dirHasIndexMD(dirAbs string) (bool, error) {
	indexPath := filepath.Join(dirAbs, "index.md")
	if isIgnoredBaseName("index.md") {
		return false, nil
	}
	st, err := os.Stat(indexPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if os.IsPermission(err) {
			return false, nil
		}
		return false, err
	}
	if st.IsDir() || !isReadablePath(indexPath) {
		return false, nil
	}
	return true, nil
}

func notFound(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
	writeHTTPString(w, "<!doctype html><html><body><h1>404 Not Found</h1></body></html>")
}

func isIgnoredBaseName(name string) bool {
	return strings.HasPrefix(name, ".")
}

func isIgnoredRelPath(rel string) bool {
	if rel == "." || rel == "" {
		return false
	}
	for _, part := range strings.Split(filepath.ToSlash(rel), "/") {
		if part == "" || part == "." {
			continue
		}
		if isIgnoredBaseName(part) {
			return true
		}
	}
	return false
}

func isReadablePath(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

func writeHTTPString(w http.ResponseWriter, s string) {
	if _, err := io.WriteString(w, s); err != nil {
		log.Printf("http response write error: %v", err)
	}
}

func writeHTTPBytes(w http.ResponseWriter, b []byte) {
	if _, err := w.Write(b); err != nil {
		log.Printf("http response write error: %v", err)
	}
}

func fatalf(msg string, args ...any) {
	log.Fatalf(msg, args...)
}
