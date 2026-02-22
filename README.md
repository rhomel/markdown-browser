# Markdown Browser and Static Site Generator

Serve or generate a directory of Markdown files as HTML.

# Examples:

```
# listen on localhost:3333, use the templates in templates/tailwind, and serve
# the current directory
go run main.go server -listen=127.0.0.1:3333 -templates=templates/tailwind .
```

```
# generate the current directory to htmls files in out/
go run main.go generate -out out .
```

# Usage

For more details see [SPEC.md](SPEC.html).

