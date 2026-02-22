package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHandleRequestRoutes(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "hello.md"), "# Hello\n\nworld\n")
	writeTestFile(t, filepath.Join(root, "articles", "a.md"), "- item\n")

	tests := []struct {
		name        string
		method      string
		target      string
		wantStatus  int
		wantType    string
		wantContain string
	}{
		{
			name:        "root index",
			method:      http.MethodGet,
			target:      "/",
			wantStatus:  http.StatusOK,
			wantType:    "text/html; charset=utf-8",
			wantContain: `href="/hello.html"`,
		},
		{
			name:        "directory index",
			method:      http.MethodGet,
			target:      "/articles",
			wantStatus:  http.StatusOK,
			wantType:    "text/html; charset=utf-8",
			wantContain: `href="/articles/a.html"`,
		},
		{
			name:        "rendered markdown html",
			method:      http.MethodGet,
			target:      "/hello.html",
			wantStatus:  http.StatusOK,
			wantType:    "text/html; charset=utf-8",
			wantContain: "<title>Hello</title>",
		},
		{
			name:        "raw markdown source",
			method:      http.MethodGet,
			target:      "/hello.md",
			wantStatus:  http.StatusOK,
			wantType:    "text/plain; charset=utf-8",
			wantContain: "# Hello",
		},
		{
			name:        "not found",
			method:      http.MethodGet,
			target:      "/missing.html",
			wantStatus:  http.StatusNotFound,
			wantContain: "404 Not Found",
		},
		{
			name:        "method not allowed",
			method:      http.MethodPost,
			target:      "/hello.html",
			wantStatus:  http.StatusMethodNotAllowed,
			wantContain: "method not allowed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.target, nil)
			rr := httptest.NewRecorder()

			handleRequest(rr, req, root)

			res := rr.Result()
			t.Cleanup(func() { _ = res.Body.Close() })
			if res.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", res.StatusCode, tc.wantStatus)
			}
			if tc.wantType != "" {
				if got := res.Header.Get("Content-Type"); got != tc.wantType {
					t.Fatalf("content-type = %q, want %q", got, tc.wantType)
				}
			}
			body := rr.Body.String()
			if !strings.Contains(body, tc.wantContain) {
				t.Fatalf("body missing %q; got %q", tc.wantContain, body)
			}
		})
	}
}

func TestGenerateAllCreatesIndexesAndRendersMarkdown(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()

	writeTestFile(t, filepath.Join(root, "hello.md"), "# Hello\n")
	writeTestFile(t, filepath.Join(root, "articles", "a.md"), "A\n")

	if err := generateAll(root, out, true); err != nil {
		t.Fatalf("generateAll error: %v", err)
	}

	assertFileContains(t, filepath.Join(out, "index.html"), `href="hello.html"`)
	assertFileContains(t, filepath.Join(out, "articles", "index.html"), `href="a.html"`)
	assertFileContains(t, filepath.Join(out, "hello.html"), "<title>Hello</title>")
	assertFileContains(t, filepath.Join(out, "articles", "a.html"), "<p>A</p>")
}

func TestGenerateAllIndexMDOverridesAutoIndex(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()

	writeTestFile(t, filepath.Join(root, "index.md"), "# Home\n")
	writeTestFile(t, filepath.Join(root, "posts", "index.md"), "# Posts Home\n")
	writeTestFile(t, filepath.Join(root, "posts", "a.md"), "A\n")

	if err := generateAll(root, out, true); err != nil {
		t.Fatalf("generateAll error: %v", err)
	}

	rootIndex := readTestFile(t, filepath.Join(out, "index.html"))
	if strings.Contains(rootIndex, "<ul>") {
		t.Fatalf("root index.html should be rendered from index.md, got auto directory index: %q", rootIndex)
	}
	if !strings.Contains(rootIndex, "<title>Home</title>") {
		t.Fatalf("root index.html missing extracted title content: %q", rootIndex)
	}

	postsIndex := readTestFile(t, filepath.Join(out, "posts", "index.html"))
	if strings.Contains(postsIndex, `href="/posts/a.html"`) {
		t.Fatalf("posts/index.html should be rendered from posts/index.md, got auto directory listing: %q", postsIndex)
	}
	if !strings.Contains(postsIndex, "<title>Posts Home</title>") {
		t.Fatalf("posts/index.html missing extracted title content: %q", postsIndex)
	}

	assertFileContains(t, filepath.Join(out, "posts", "a.html"), "<p>A</p>")
}

func TestIgnoredDotFilesAndDirectoriesAreSkippedAnd404(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()

	writeTestFile(t, filepath.Join(root, "visible.md"), "# Visible\n")
	writeTestFile(t, filepath.Join(root, ".hidden.md"), "# Hidden\n")
	writeTestFile(t, filepath.Join(root, ".secret", "a.md"), "# Secret\n")
	writeTestFile(t, filepath.Join(root, "posts", ".draft.md"), "# Draft\n")

	if err := generateAll(root, out, true); err != nil {
		t.Fatalf("generateAll error: %v", err)
	}

	assertFileContains(t, filepath.Join(out, "index.html"), `href="visible.html"`)
	if _, err := os.Stat(filepath.Join(out, ".hidden.html")); !os.IsNotExist(err) {
		t.Fatalf("unexpected generated hidden file html: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(out, ".secret")); !os.IsNotExist(err) {
		t.Fatalf("unexpected generated hidden directory: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "posts", ".draft.html")); !os.IsNotExist(err) {
		t.Fatalf("unexpected generated hidden nested file html: err=%v", err)
	}

	for _, target := range []string{"/.hidden.html", "/.hidden.md", "/.secret", "/.secret/a.html", "/posts/.draft.html"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		handleRequest(rr, req, root)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, rr.Code)
		}
	}
}

func TestUnreadableFilesAreIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit test is POSIX-specific")
	}

	root := t.TempDir()
	out := t.TempDir()

	writeTestFile(t, filepath.Join(root, "visible.md"), "# Visible\n")
	unreadable := filepath.Join(root, "private.md")
	writeTestFile(t, unreadable, "# Private\n")

	if err := os.Chmod(unreadable, 0o000); err != nil {
		t.Fatalf("chmod unreadable file: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(unreadable, 0o644) })

	if isReadablePath(unreadable) {
		t.Skip("filesystem/runtime does not enforce unreadable test file permissions")
	}

	if err := generateAll(root, out, true); err != nil {
		t.Fatalf("generateAll error: %v", err)
	}

	assertFileContains(t, filepath.Join(out, "index.html"), `href="visible.html"`)
	indexHTML := readTestFile(t, filepath.Join(out, "index.html"))
	if strings.Contains(indexHTML, `href="private.html"`) {
		t.Fatalf("unreadable file should not appear in generated index: %q", indexHTML)
	}
	if _, err := os.Stat(filepath.Join(out, "private.html")); !os.IsNotExist(err) {
		t.Fatalf("unexpected generated html for unreadable file: err=%v", err)
	}

	for _, target := range []string{"/private.html", "/private.md"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		handleRequest(rr, req, root)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, rr.Code)
		}
	}
}

func TestGenerateSkipsOutputDirectoryWhenNestedInInput(t *testing.T) {
	root := t.TempDir()
	out := filepath.Join(root, "out")

	writeTestFile(t, filepath.Join(root, "hello.md"), "# Hello\n")
	writeTestFile(t, filepath.Join(root, "out", "existing.md"), "# Existing\n")

	if err := generateAll(root, out, true); err != nil {
		t.Fatalf("generateAll error: %v", err)
	}

	indexHTML := readTestFile(t, filepath.Join(out, "index.html"))
	if strings.Contains(indexHTML, `href="out"`) || strings.Contains(indexHTML, `href="out/existing.html"`) {
		t.Fatalf("generated root index should exclude nested output directory tree: %q", indexHTML)
	}
	if _, err := os.Stat(filepath.Join(out, "existing.html")); !os.IsNotExist(err) {
		t.Fatalf("generator should not process markdown files inside nested output directory: err=%v", err)
	}
	assertFileContains(t, filepath.Join(out, "hello.html"), "<title>Hello</title>")
}

func TestMDIgnorePatternsAffectServerAndGenerate(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()

	writeTestFile(t, filepath.Join(root, ".mdignore"), "out\n*.draft.md\nignored-dir/\n")
	writeTestFile(t, filepath.Join(root, "hello.md"), "# Hello\n")
	writeTestFile(t, filepath.Join(root, "pending-article.draft.md"), "# Draft\n")
	writeTestFile(t, filepath.Join(root, "ignored-dir", "note.md"), "# Ignored Dir\n")
	writeTestFile(t, filepath.Join(root, "posts", "keep.md"), "# Keep\n")

	if err := generateAll(root, out, true); err != nil {
		t.Fatalf("generateAll error: %v", err)
	}

	indexHTML := readTestFile(t, filepath.Join(out, "index.html"))
	if strings.Contains(indexHTML, "pending-article.draft") {
		t.Fatalf("draft markdown matched by .mdignore should be excluded from generated index: %q", indexHTML)
	}
	if strings.Contains(indexHTML, "ignored-dir") {
		t.Fatalf("ignored directory matched by .mdignore should be excluded from generated index: %q", indexHTML)
	}
	if _, err := os.Stat(filepath.Join(out, "pending-article.draft.html")); !os.IsNotExist(err) {
		t.Fatalf("unexpected generated html for .mdignore-matched file: err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "ignored-dir")); !os.IsNotExist(err) {
		t.Fatalf("unexpected generated output for .mdignore-matched directory: err=%v", err)
	}

	for _, target := range []string{"/pending-article.draft.html", "/pending-article.draft.md", "/ignored-dir", "/ignored-dir/note.html"} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		handleRequest(rr, req, root)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404", target, rr.Code)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/posts/keep.html", nil)
	rr := httptest.NewRecorder()
	handleRequest(rr, req, root)
	if rr.Code != http.StatusOK {
		t.Fatalf("kept file status = %d, want 200", rr.Code)
	}
}

func TestOptionalTemplatesOverridePageWrappers(t *testing.T) {
	root := t.TempDir()
	tmplDir := t.TempDir()

	writeTestFile(t, filepath.Join(root, "hello.md"), "# Hello\n\nBody\n")
	writeTestFile(t, filepath.Join(tmplDir, "page.html"), `P|{{.Title}}|{{.Body}}`)
	writeTestFile(t, filepath.Join(tmplDir, "directory.html"), `D|{{.Title}}|{{.Body}}`)
	writeTestFile(t, filepath.Join(tmplDir, "article.html"), `A|{{.Title}}|{{.Body}}`)
	writeTestFile(t, filepath.Join(tmplDir, "error.html"), `E|{{.Title}}|{{.Body}}`)

	orig := activeTemplates
	t.Cleanup(func() { activeTemplates = orig })
	if err := setActiveTemplates(tmplDir); err != nil {
		t.Fatalf("setActiveTemplates error: %v", err)
	}

	dirPage, err := renderDirectoryHTML(root, "")
	if err != nil {
		t.Fatalf("renderDirectoryHTML error: %v", err)
	}
	if !strings.HasPrefix(dirPage, "D|Index|") {
		t.Fatalf("directory wrapper not applied: %q", dirPage)
	}

	articlePage, err := renderMarkdownPage(root, "hello.md")
	if err != nil {
		t.Fatalf("renderMarkdownPage error: %v", err)
	}
	if !strings.HasPrefix(articlePage, "A|Hello|") {
		t.Fatalf("article wrapper not applied: %q", articlePage)
	}
	if strings.Contains(articlePage, "<h1>Hello</h1>") {
		t.Fatalf("leading H1 should be moved to Title and removed from article body: %q", articlePage)
	}

	req := httptest.NewRequest(http.MethodGet, "/missing.html", nil)
	rr := httptest.NewRecorder()
	handleRequest(rr, req, root)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing route status = %d, want 404", rr.Code)
	}
	if !strings.HasPrefix(rr.Body.String(), "E|404 Not Found|") {
		t.Fatalf("error wrapper not applied: %q", rr.Body.String())
	}
}

func TestSplitLeadingMarkdownTitle(t *testing.T) {
	title, body := splitLeadingMarkdownTitle("\n# Welcome!\n\nHello\n\n# Next\n")
	if title != "Welcome!" {
		t.Fatalf("title = %q, want %q", title, "Welcome!")
	}
	if body != "Hello\n\n# Next\n" {
		t.Fatalf("body = %q", body)
	}
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func assertFileContains(t *testing.T, path, want string) {
	t.Helper()
	got := readTestFile(t, path)
	if !strings.Contains(got, want) {
		t.Fatalf("%s missing %q; got %q", path, want, got)
	}
}
