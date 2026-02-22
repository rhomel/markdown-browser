package main

import (
	"bufio"
	"bytes"
	"errors"
	"flag"
	"fmt"
	"html"
	htemplate "html/template"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yuin/goldmark"
	gast "github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/text"
)

type treeEntry struct {
	Name      string
	RelPath   string
	IsDir     bool
	IsIndexMD bool
	Children  []treeEntry
}

type pageKind string

const (
	pageKindPage      pageKind = "page"
	pageKindDirectory pageKind = "directory"
	pageKindArticle   pageKind = "article"
	pageKindError     pageKind = "error"
)

type pageTemplateData struct {
	Title             string
	Body              htemplate.HTML
	ModifyTimeLocale  string
	ModifyTimeISO8601 string
}

type pageTemplates struct {
	page      *htemplate.Template
	directory *htemplate.Template
	article   *htemplate.Template
	errPage   *htemplate.Template
}

var (
	errPathEscape      = errors.New("path escape")
	errIgnoredPath     = errors.New("ignored path")
	errPathNotReadable = errors.New("path not readable")
	activeTemplates    = mustLoadPageTemplates("")
)

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
	templatesDir := fsFlags.String("templates", "", "template directory")
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
	if err := setActiveTemplates(*templatesDir); err != nil {
		fatalf("load templates: %v", err)
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
	if isIgnoredRelPath(rootAbs, rel) {
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
	overwrite := fsFlags.Bool("overwrite", true, "overwrite existing output files")
	templatesDir := fsFlags.String("templates", "", "template directory")
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
	if err := setActiveTemplates(*templatesDir); err != nil {
		fatalf("load templates: %v", err)
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
	if dirExistsAndNotEmpty(outAbs) {
		if !confirmOverwrite(outAbs, *overwrite) {
			log.Printf("generate cancelled")
			return
		}
	}

	if err := os.MkdirAll(outAbs, 0o755); err != nil {
		fatalf("create output dir: %v", err)
	}

	if err := generateAll(rootAbs, outAbs, *overwrite); err != nil {
		fatalf("generate failed: %v", err)
	}
}

func generateAll(rootAbs, outAbs string, overwrite bool) error {
	excludedRel := relIfWithin(rootAbs, outAbs)

	hasRootIndexMD, err := dirHasIndexMD(rootAbs, rootAbs)
	if err != nil {
		return err
	}
	if !hasRootIndexMD {
		data, err := renderDirectoryHTMLGenerate(rootAbs, "", excludedRel)
		if err != nil {
			return err
		}
		outFile := filepath.Join(outAbs, "index.html")
		if err := writeFileMaybeOverwrite(outFile, []byte(data), overwrite); err != nil {
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
		rel = filepath.ToSlash(rel)
		if excludedRel != "" && (rel == excludedRel || strings.HasPrefix(rel, excludedRel+"/")) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if isIgnoredRelPath(rootAbs, rel) {
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

			hasIndexMD, err := dirHasIndexMD(rootAbs, pathAbs)
			if err != nil {
				return err
			}
			if hasIndexMD {
				return nil
			}

			data, err := renderDirectoryHTMLGenerate(rootAbs, rel, excludedRel)
			if err != nil {
				return err
			}
			outFile := filepath.Join(outDirPath, "index.html")
			if err := writeFileMaybeOverwrite(outFile, []byte(data), overwrite); err != nil {
				return err
			}
			fmt.Println(outFile)
			return nil
		}

		if strings.EqualFold(filepath.Base(rel), "index.md") {
			outFile := filepath.Join(outAbs, filepath.Dir(rel), "index.html")
			return renderMDFileTo(rootAbs, rel, outFile, overwrite)
		}

		if strings.HasSuffix(rel, ".md") {
			outFile := filepath.Join(outAbs, strings.TrimSuffix(rel, ".md")+".html")
			return renderMDFileTo(rootAbs, rel, outFile, overwrite)
		}

		return nil
	})
}

func renderMDFileTo(rootAbs, mdRel, outFile string, overwrite bool) error {
	pageHTML, err := renderMarkdownPage(rootAbs, mdRel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(outFile), 0o755); err != nil {
		return err
	}
	if err := writeFileMaybeOverwrite(outFile, []byte(pageHTML), overwrite); err != nil {
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
	if isIgnoredRelPath(rootAbs, mdRel) {
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
		if isNotFoundRenderError(err) {
			notFound(w)
			return
		}
		log.Printf("render markdown error for %q: %v", mdRel, err)
		serveErrorPage(w, http.StatusInternalServerError, "Internal Server Error")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	writeHTTPString(w, pageHTML)
}

func renderDirectoryHTML(rootAbs, relDir string) (string, error) {
	return renderDirectoryHTMLWithOptions(rootAbs, relDir, "", true)
}

func renderDirectoryHTMLGenerate(rootAbs, relDir, excludedRel string) (string, error) {
	return renderDirectoryHTMLWithOptions(rootAbs, relDir, excludedRel, false)
}

func renderDirectoryHTMLWithOptions(rootAbs, relDir, excludedRel string, absoluteLinks bool) (string, error) {
	dirAbs := filepath.Join(rootAbs, filepath.FromSlash(relDir))
	if err := ensureDir(dirAbs); err != nil {
		return "", err
	}
	if relDir != "" && isIgnoredRelPath(rootAbs, relDir) {
		return "", errIgnoredPath
	}
	if !isReadablePath(dirAbs) {
		return "", errPathNotReadable
	}

	entries, err := buildTree(rootAbs, relDir, excludedRel)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	renderTreeHTML(&b, entries, relDir, absoluteLinks)
	return renderWrappedPage(pageKindDirectory, "Index", b.String())
}

func buildTree(rootAbs, relDir, excludedRel string) ([]treeEntry, error) {
	dirAbs := filepath.Join(rootAbs, filepath.FromSlash(relDir))
	list, err := os.ReadDir(dirAbs)
	if err != nil {
		return nil, err
	}

	var out []treeEntry
	for _, e := range list {
		name := e.Name()
		relChild := filepath.ToSlash(filepath.Join(relDir, name))
		if isIgnoredRelPath(rootAbs, relChild) {
			continue
		}
		if excludedRel != "" && (relChild == excludedRel || strings.HasPrefix(relChild, excludedRel+"/")) {
			continue
		}
		childAbs := filepath.Join(rootAbs, filepath.FromSlash(relChild))
		if e.IsDir() {
			if !isReadablePath(childAbs) {
				continue
			}
			children, err := buildTree(rootAbs, relChild, excludedRel)
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

func renderTreeHTML(b *strings.Builder, entries []treeEntry, pageRelDir string, absoluteLinks bool) {
	b.WriteString("<ul>")
	for _, e := range entries {
		if e.IsIndexMD {
			continue
		}
		b.WriteString("<li>")
		href := treeEntryHref(pageRelDir, e.RelPath, absoluteLinks)
		if e.IsDir {
			b.WriteString(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(e.Name) + `</a>`)
			renderTreeHTML(b, e.Children, pageRelDir, absoluteLinks)
		} else {
			b.WriteString(`<a href="` + html.EscapeString(href) + `">` + html.EscapeString(e.Name) + `</a>`)
		}
		b.WriteString("</li>")
	}
	b.WriteString("</ul>")
}

func renderMarkdownFile(rootAbs, mdRel string) (string, error) {
	_, body, _, err := renderMarkdownContent(rootAbs, mdRel)
	return body, err
}

func renderMarkdownContent(rootAbs, mdRel string) (title string, htmlBody string, modTime time.Time, err error) {
	mdRel = filepath.ToSlash(filepath.Clean(filepath.FromSlash(mdRel)))
	abs := filepath.Join(rootAbs, filepath.FromSlash(mdRel))
	if !isWithinRoot(rootAbs, abs) {
		return "", "", time.Time{}, errPathEscape
	}
	if isIgnoredRelPath(rootAbs, mdRel) {
		return "", "", time.Time{}, errIgnoredPath
	}
	if !isReadablePath(abs) {
		return "", "", time.Time{}, errPathNotReadable
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", "", time.Time{}, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", "", time.Time{}, err
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM),
	)
	doc := md.Parser().Parse(text.NewReader(data))
	title = extractAndRemoveLeadingH1(doc, data)

	var out strings.Builder
	if err := md.Renderer().Render(&out, data, doc); err != nil {
		return "", "", time.Time{}, err
	}
	return title, out.String(), st.ModTime(), nil
}

func renderMarkdownPage(rootAbs, mdRel string) (string, error) {
	title, htmlBody, modTime, err := renderMarkdownContent(rootAbs, mdRel)
	if err != nil {
		return "", err
	}
	return renderWrappedPageWithModTime(pageKindArticle, title, htmlBody, &modTime)
}

func wrapHTMLPageWithTitle(title, body string) (string, error) {
	return renderWrappedPage(pageKindPage, title, body)
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

func dirHasIndexMD(rootAbs, dirAbs string) (bool, error) {
	indexPath := filepath.Join(dirAbs, "index.md")
	relIndex, err := filepath.Rel(rootAbs, indexPath)
	if err == nil && isIgnoredRelPath(rootAbs, filepath.ToSlash(relIndex)) {
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
	serveErrorPage(w, http.StatusNotFound, "404 Not Found")
}

func isIgnoredBaseName(name string) bool {
	return strings.HasPrefix(name, ".")
}

func isIgnoredRelPath(rootAbs, rel string) bool {
	if rel == "." || rel == "" {
		return false
	}
	rel = filepath.ToSlash(rel)
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." {
			continue
		}
		if isIgnoredBaseName(part) {
			return true
		}
	}
	if matchesMDIgnore(rootAbs, rel) {
		return true
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

func matchesMDIgnore(rootAbs, rel string) bool {
	patterns, err := readMDIgnorePatterns(rootAbs)
	if err != nil {
		return false
	}
	return matchMDIgnore(patterns, filepath.ToSlash(rel))
}

func readMDIgnorePatterns(rootAbs string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(rootAbs, ".mdignore"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || os.IsPermission(err) {
			return nil, nil
		}
		return nil, err
	}
	var patterns []string
	sc := bufio.NewScanner(strings.NewReader(string(data)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, filepath.ToSlash(line))
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return patterns, nil
}

func matchMDIgnore(patterns []string, rel string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	parts := strings.Split(rel, "/")
	for _, pat := range patterns {
		if pat == "" {
			continue
		}
		dirOnly := strings.HasSuffix(pat, "/")
		pat = strings.TrimSuffix(pat, "/")
		if pat == "" {
			continue
		}
		if strings.Contains(pat, "/") {
			if mdignorePathMatch(pat, rel) {
				return true
			}
			if dirOnly && strings.HasPrefix(rel, pat+"/") {
				return true
			}
			continue
		}
		for i, part := range parts {
			if mdignorePathMatch(pat, part) {
				if dirOnly && i == len(parts)-1 {
					// best-effort: treat trailing slash patterns as directory names; callers use path segments for dirs/children.
				}
				return true
			}
		}
	}
	return false
}

func mdignorePathMatch(pattern, value string) bool {
	ok, err := path.Match(pattern, value)
	if err != nil {
		return false
	}
	return ok
}

func isNotFoundRenderError(err error) bool {
	return errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, errPathEscape) ||
		errors.Is(err, errIgnoredPath) ||
		errors.Is(err, errPathNotReadable)
}

func serveErrorPage(w http.ResponseWriter, status int, message string) {
	w.WriteHeader(status)
	body := "<h1>" + html.EscapeString(message) + "</h1>"
	page, err := renderWrappedPage(pageKindError, message, body)
	if err != nil {
		log.Printf("render error page failed: %v", err)
		writeHTTPString(w, "<!doctype html><html><body>"+body+"</body></html>")
		return
	}
	writeHTTPString(w, page)
}

func renderWrappedPage(kind pageKind, title, body string) (string, error) {
	return activeTemplates.render(kind, title, body, nil)
}

func renderWrappedPageWithModTime(kind pageKind, title, body string, modTime *time.Time) (string, error) {
	return activeTemplates.render(kind, title, body, modTime)
}

func extractAndRemoveLeadingH1(doc gast.Node, source []byte) string {
	heading, ok := doc.FirstChild().(*gast.Heading)
	if !ok || heading.Level != 1 {
		return ""
	}
	title := strings.TrimSpace(extractNodeText(heading, source))
	doc.RemoveChild(doc, heading)
	return title
}

func extractNodeText(node gast.Node, source []byte) string {
	var b strings.Builder
	_ = gast.Walk(node, func(n gast.Node, entering bool) (gast.WalkStatus, error) {
		if !entering {
			return gast.WalkContinue, nil
		}
		switch v := n.(type) {
		case *gast.Text:
			b.Write(v.Segment.Value(source))
			if v.HardLineBreak() || v.SoftLineBreak() {
				b.WriteByte(' ')
			}
		case *gast.String:
			b.Write(v.Value)
		}
		return gast.WalkContinue, nil
	})
	return b.String()
}

func treeEntryHref(pageRelDir, entryRelPath string, absoluteLinks bool) string {
	if absoluteLinks {
		return "/" + strings.TrimPrefix(entryRelPath, "./")
	}
	if pageRelDir == "" {
		return strings.TrimPrefix(entryRelPath, "./")
	}
	rel, err := filepath.Rel(filepath.FromSlash(pageRelDir), filepath.FromSlash(entryRelPath))
	if err != nil {
		return strings.TrimPrefix(entryRelPath, "./")
	}
	return filepath.ToSlash(rel)
}

func writeHTTPString(w http.ResponseWriter, s string) {
	if _, err := io.WriteString(w, s); err != nil {
		log.Printf("http response write error: %v", err)
	}
}

func setActiveTemplates(dir string) error {
	t, err := loadPageTemplates(dir)
	if err != nil {
		return err
	}
	activeTemplates = t
	return nil
}

func mustLoadPageTemplates(dir string) *pageTemplates {
	t, err := loadPageTemplates(dir)
	if err != nil {
		panic(err)
	}
	return t
}

func loadPageTemplates(dir string) (*pageTemplates, error) {
	pageTmpl, err := htemplate.New("page").Parse(defaultPageTemplateText())
	if err != nil {
		return nil, err
	}
	pt := &pageTemplates{page: pageTmpl}
	if dir == "" {
		return pt, nil
	}
	if err := ensureDir(dir); err != nil {
		return nil, err
	}
	if pt.page, err = loadTemplateFileOrFallback(filepath.Join(dir, "page.html"), pt.page); err != nil {
		return nil, err
	}
	if pt.directory, err = loadTemplateFileOrFallback(filepath.Join(dir, "directory.html"), nil); err != nil {
		return nil, err
	}
	if pt.article, err = loadTemplateFileOrFallback(filepath.Join(dir, "article.html"), nil); err != nil {
		return nil, err
	}
	if pt.errPage, err = loadTemplateFileOrFallback(filepath.Join(dir, "error.html"), nil); err != nil {
		return nil, err
	}
	return pt, nil
}

func loadTemplateFileOrFallback(path string, fallback *htemplate.Template) (*htemplate.Template, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fallback, nil
		}
		return nil, err
	}
	return htemplate.New(filepath.Base(path)).Parse(string(data))
}

func (p *pageTemplates) render(kind pageKind, title, body string, modTime *time.Time) (string, error) {
	tmpl := p.page
	switch kind {
	case pageKindDirectory:
		if p.directory != nil {
			tmpl = p.directory
		}
	case pageKindArticle:
		if p.article != nil {
			tmpl = p.article
		}
	case pageKindError:
		if p.errPage != nil {
			tmpl = p.errPage
		}
	}
	var buf bytes.Buffer
	data := pageTemplateData{
		Title: title,
		Body:  htemplate.HTML(body),
	}
	if modTime != nil && !modTime.IsZero() {
		local := modTime.Local()
		data.ModifyTimeLocale = local.Format(time.RFC1123)
		data.ModifyTimeISO8601 = local.Format(time.RFC3339)
	}
	err := tmpl.Execute(&buf, pageTemplateData{
		Title:             data.Title,
		Body:              data.Body,
		ModifyTimeLocale:  data.ModifyTimeLocale,
		ModifyTimeISO8601: data.ModifyTimeISO8601,
	})
	if err != nil {
		return "", err
	}
	return buf.String(), nil
}

func defaultPageTemplateText() string {
	return `<!doctype html><html><head><meta charset="utf-8">{{if .Title}}<title>{{.Title}}</title>{{end}}</head><body>{{.Body}}</body></html>`
}

func writeHTTPBytes(w http.ResponseWriter, b []byte) {
	if _, err := w.Write(b); err != nil {
		log.Printf("http response write error: %v", err)
	}
}

func writeFileMaybeOverwrite(path string, data []byte, overwrite bool) error {
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return os.WriteFile(path, data, 0o644)
}

func relIfWithin(rootAbs, maybeChildAbs string) string {
	if !isWithinRoot(rootAbs, maybeChildAbs) || sameDir(rootAbs, maybeChildAbs) {
		return ""
	}
	rel, err := filepath.Rel(rootAbs, maybeChildAbs)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

func dirExistsAndNotEmpty(dir string) bool {
	f, err := os.Open(dir)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	_, err = f.Readdirnames(1)
	return err == nil
}

func confirmOverwrite(outAbs string, overwrite bool) bool {
	modeText := "overwrite existing files"
	if !overwrite {
		modeText = "not overwrite existing files"
	}
	fmt.Fprintf(os.Stdout, "Output directory %s already contains files. Continue? Existing files may be overwritten (%s) [y/N]: ", outAbs, modeText)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func fatalf(msg string, args ...any) {
	log.Fatalf(msg, args...)
}
