package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
			wantContain: "<h1>Hello</h1>",
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

	if err := generateAll(root, out); err != nil {
		t.Fatalf("generateAll error: %v", err)
	}

	assertFileContains(t, filepath.Join(out, "index.html"), `href="/hello.html"`)
	assertFileContains(t, filepath.Join(out, "articles", "index.html"), `href="/articles/a.html"`)
	assertFileContains(t, filepath.Join(out, "hello.html"), "<h1>Hello</h1>")
	assertFileContains(t, filepath.Join(out, "articles", "a.html"), "<p>A</p>")
}

func TestGenerateAllIndexMDOverridesAutoIndex(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()

	writeTestFile(t, filepath.Join(root, "index.md"), "# Home\n")
	writeTestFile(t, filepath.Join(root, "posts", "index.md"), "# Posts Home\n")
	writeTestFile(t, filepath.Join(root, "posts", "a.md"), "A\n")

	if err := generateAll(root, out); err != nil {
		t.Fatalf("generateAll error: %v", err)
	}

	rootIndex := readTestFile(t, filepath.Join(out, "index.html"))
	if strings.Contains(rootIndex, "<ul>") {
		t.Fatalf("root index.html should be rendered from index.md, got auto directory index: %q", rootIndex)
	}
	if !strings.Contains(rootIndex, "<h1>Home</h1>") {
		t.Fatalf("root index.html missing rendered markdown content: %q", rootIndex)
	}

	postsIndex := readTestFile(t, filepath.Join(out, "posts", "index.html"))
	if strings.Contains(postsIndex, `href="/posts/a.html"`) {
		t.Fatalf("posts/index.html should be rendered from posts/index.md, got auto directory listing: %q", postsIndex)
	}
	if !strings.Contains(postsIndex, "<h1>Posts Home</h1>") {
		t.Fatalf("posts/index.html missing rendered markdown content: %q", postsIndex)
	}

	assertFileContains(t, filepath.Join(out, "posts", "a.html"), "<p>A</p>")
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
