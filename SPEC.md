
Markdown Browser will consist of a single Go command that:

- Starts an HTTP server with the "server" command
- Generates HTML corresponding HTML files with the "generate" command

# server command

The `server` command will require a single command line argument which is the
path to the directory tree of markdown files.

Once started the server will listen on the 0.0.0.0:3333 interface and port by
default. An optional flag `-listen` may take a custom interface/address
instead. For example `-listen=localhost:3333` will instead listen on
127.0.0.1:3333.

Once started the server will accept HTTP requests.

The special '/' resource will serve a generated HTML index of the directory
tree of markdown files. For example if the directory given is '.' and the
current directory has the following files sorted alphabetically:

- about.md
- articles/
  - 2026-02-20-snowboard-basics.md
  - 2026-02-22-golf-swing-basics.md
- hello.md

Then an HTML page with an HTML directory tree view similar to the following
should be generated:

```
<ul>
  <li><a href="/about.html">about</li>
  <li>
    <a href="/articles">articles</a>
    <ul>
      <li><a href="2026-02-20-snowboard-basics.html">2026-02-20-snowboard-basics</li>
      <li><a href="2026-02-22-golf-swing-basics.html">2026-02-22-golf-swing-basics</li>
    </ul>
  </li>
  <li><a href="/hello.html">hello</li>
</ul>
```

When the server receives a GET request for `/hello.html` the server should
return the generated HTML for the `hello.md` markdown file.

When the server receives a GET request for a directory like `/articles` or
`/articles/` then the server should return a similar HTML directory tree view
as the '/' example above except rooted at /articles/.

When the server receives a GET request for `/hello.md` then it should respond
with the plaintext of the source markdown file.

If no resource exists for an HTTP request then an HTTP 404 response and page
should be returned.

The HTTP server should log each HTTP request to STDOUT.

The HTTP server should also emit a starting log with the http server URL where
it is listening.

# generate command

The generate command retains the HTML rendering behavior except instead of
starting and HTTP server and waiting for requests to render markdown to html on
demand, it will render the approppriate pages including directory tree HTML
pages into a corresponding output directory.

The output directory flag will be a required argument and the directory cannot
resolve the the same directory as the current directory.

If an `index.md` file is present in any directory, then the generate command
will avoid generating an automated index.html directory tree list and instead
render the index.md file to index.html.

The generate command should print to STDOUT each file it processes as it
generates them.

# Dependencies

Module dependencies should be avoided as much as possible.

For markdown the github.com/yuin/goldmark library may be used.

# Ignored Files

Ignored files and directories should not be included in either `server` or
`generate` command modes. When in `server` mode the request should result in a
404 response.

The following conditions should cause a file or directory to be ignored:

- dot files and directories; names beginning with a '.' like '.git' should
  always be ignored and skipped.
- files and directories without read permission

