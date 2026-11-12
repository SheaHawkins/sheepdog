// Package script compiles graph files (TypeScript) and runs them in goja.
//
// A graph file is not a valid ES module as written: it mixes `export const
// meta`, top-level `await` and top-level `return`, and no module format allows
// all three. Compile therefore splits the file into a header (import
// statements and the meta declaration) and a body, and wraps the body in an
// async function before handing the result to esbuild:
//
//	<imports and meta, verbatim>;export default async function __sheepdog_main() { <body> }
//
// The wrapper opens on the same line the body starts on, so line numbers in
// the intermediate file match the original and the inline source map esbuild
// emits points back at the user's file.
package script

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/evanw/esbuild/pkg/api"
)

// Program is a compiled graph: one self-contained JavaScript file.
type Program struct {
	Path string // absolute path of the entry file
	Code string // bundled JavaScript, with an inline source map
}

// globalName is the variable the bundle assigns its exports to.
const globalName = "__sheepdog_graph"

// Compile strips types from the graph at path, bundles its relative imports
// and returns JavaScript that goja can run.
func Compile(path string) (*Program, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	header, body, err := splitHeader(string(src))
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	wrapped := header + ";export default async function __sheepdog_main() {" + body + "\n}\n"

	res := api.Build(api.BuildOptions{
		Stdin: &api.StdinOptions{
			Contents:   wrapped,
			ResolveDir: filepath.Dir(abs),
			Sourcefile: abs,
			Loader:     api.LoaderTS,
		},
		// Source map paths are relative to this, and goja resolves them
		// against the entry file's directory, so the two must agree.
		AbsWorkingDir: filepath.Dir(abs),
		Bundle:        true,
		Write:         false,
		Format:        api.FormatIIFE,
		GlobalName:    globalName,
		// goja implements async functions natively, so they are left alone.
		Target:    api.ES2017,
		Platform:  api.PlatformNeutral,
		Sourcemap: api.SourceMapInline,
		// Graph files have no npm packages: anything that is not a relative
		// import is an error rather than a lookup in node_modules.
		Plugins:  []api.Plugin{relativeImportsOnly},
		LogLevel: api.LogLevelSilent,
	})
	if len(res.Errors) > 0 {
		return nil, formatMessages(res.Errors, strings.Count(string(src), "\n")+1)
	}
	return &Program{Path: abs, Code: string(res.OutputFiles[0].Contents)}, nil
}

var relativeImportsOnly = api.Plugin{
	Name: "relative-imports-only",
	Setup: func(b api.PluginBuild) {
		b.OnResolve(api.OnResolveOptions{Filter: `^[^./]`}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
			return api.OnResolveResult{}, fmt.Errorf("graph files can only import relative paths, not %q", args.Path)
		})
	},
}

// formatMessages renders esbuild errors. An error past the last line of the
// original file is in the wrapper's closing brace, which means the user's
// code ended early (an unclosed bracket or a dangling expression).
func formatMessages(msgs []api.Message, lines int) error {
	var b strings.Builder
	for i, m := range msgs {
		if i > 0 {
			b.WriteString("\n")
		}
		text := m.Text
		if m.Location != nil {
			line, col := m.Location.Line, m.Location.Column+1
			if line > lines {
				line, col, text = lines, 1, "unexpected end of file"
			}
			fmt.Fprintf(&b, "%s:%d:%d: ", m.Location.File, line, col)
		}
		b.WriteString(text)
	}
	return fmt.Errorf("%s", b.String())
}

// splitHeader separates the leading import statements and the meta
// declaration from the rest of the file. It understands just enough
// JavaScript to find where those statements end: whitespace, comments,
// strings, template literals and bracket nesting.
func splitHeader(src string) (header, body string, err error) {
	s := &scanner{src: src}
	sawMeta := false
	for {
		start := s.pos
		s.skipSpace()
		switch {
		case s.keyword("import"):
			if err := s.skipImport(); err != nil {
				return "", "", err
			}
		case s.keyword("export"):
			save := s.pos
			s.pos += len("export")
			s.skipSpace()
			if !s.keyword("const") {
				s.pos = save
				return "", "", s.errorf("only `export const meta` may be exported from a graph")
			}
			s.pos += len("const")
			s.skipSpace()
			if !s.keyword("meta") {
				s.pos = save
				return "", "", s.errorf("only `export const meta` may be exported from a graph")
			}
			if sawMeta {
				return "", "", s.errorf("meta is declared twice")
			}
			if err := s.skipStatement(); err != nil {
				return "", "", err
			}
			sawMeta = true
		default:
			s.pos = start
			if !sawMeta {
				s.skipSpace()
				return "", "", s.errorf("a graph must begin with `export const meta = {...}` (after any imports)")
			}
			return src[:s.pos], src[s.pos:], nil
		}
	}
}

type scanner struct {
	src string
	pos int
}

func (s *scanner) errorf(format string, args ...any) error {
	line := 1 + strings.Count(s.src[:s.pos], "\n")
	return fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, args...))
}

func (s *scanner) peek(n int) byte {
	if s.pos+n < len(s.src) {
		return s.src[s.pos+n]
	}
	return 0
}

// keyword reports whether the identifier kw starts at the current position.
func (s *scanner) keyword(kw string) bool {
	if !strings.HasPrefix(s.src[s.pos:], kw) {
		return false
	}
	return !isIdent(s.peek(len(kw)))
}

func isIdent(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func (s *scanner) skipSpace() {
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			s.pos++
		case c == '/' && s.peek(1) == '/':
			for s.pos < len(s.src) && s.src[s.pos] != '\n' {
				s.pos++
			}
		case c == '/' && s.peek(1) == '*':
			end := strings.Index(s.src[s.pos+2:], "*/")
			if end < 0 {
				s.pos = len(s.src)
				return
			}
			s.pos += end + 4
		default:
			return
		}
	}
}

// skipImport consumes one import statement, which ends at its module
// specifier string (plus an optional semicolon).
func (s *scanner) skipImport() error {
	s.pos += len("import")
	depth := 0
	for {
		s.skipSpace()
		if s.pos >= len(s.src) {
			return s.errorf("unterminated import")
		}
		switch c := s.src[s.pos]; c {
		case '{':
			depth++
			s.pos++
		case '}':
			depth--
			s.pos++
		case '\'', '"':
			if err := s.skipString(c); err != nil {
				return err
			}
			if depth == 0 {
				s.skipSpace()
				if s.peek(0) == ';' {
					s.pos++
				}
				return nil
			}
		case ';', '\n':
			return s.errorf("import without a module specifier")
		default:
			s.pos++
		}
	}
}

// skipStatement consumes `meta = <expression>` up to the end of the
// statement: a semicolon or newline outside any brackets, once the
// expression has started.
func (s *scanner) skipStatement() error {
	depth := 0
	started := false
	for s.pos < len(s.src) {
		c := s.src[s.pos]
		switch {
		case c == '/' && (s.peek(1) == '/' || s.peek(1) == '*'):
			if depth == 0 && started && s.peek(1) == '/' {
				return nil
			}
			s.skipSpace()
			continue
		case c == '\'' || c == '"':
			if err := s.skipString(c); err != nil {
				return err
			}
			started = true
			continue
		case c == '`':
			if err := s.skipTemplate(); err != nil {
				return err
			}
			started = true
			continue
		case c == '{' || c == '[' || c == '(':
			depth++
			started = true
		case c == '}' || c == ']' || c == ')':
			depth--
			if depth < 0 {
				return s.errorf("unbalanced %q in meta", c)
			}
		case c == ';' && depth == 0:
			s.pos++
			return nil
		case c == '\n' && depth == 0 && started:
			// A newline ends the statement unless the next line continues
			// it, as in `}\n  as const` or a trailing operator.
			save := s.pos
			s.skipSpace()
			if s.keyword("as") || s.keyword("satisfies") {
				continue
			}
			s.pos = save
			return nil
		}
		s.pos++
	}
	if depth != 0 {
		return s.errorf("unterminated meta")
	}
	return nil
}

func (s *scanner) skipString(quote byte) error {
	s.pos++
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
			continue
		case quote:
			s.pos++
			return nil
		case '\n':
			return s.errorf("unterminated string")
		}
		s.pos++
	}
	return s.errorf("unterminated string")
}

func (s *scanner) skipTemplate() error {
	s.pos++
	for s.pos < len(s.src) {
		switch s.src[s.pos] {
		case '\\':
			s.pos += 2
			continue
		case '`':
			s.pos++
			return nil
		case '$':
			if s.peek(1) == '{' {
				s.pos += 2
				// Scan the substitution up to its closing brace.
				depth := 1
				for s.pos < len(s.src) && depth > 0 {
					switch c := s.src[s.pos]; c {
					case '{':
						depth++
					case '}':
						depth--
					case '\'', '"':
						if err := s.skipString(c); err != nil {
							return err
						}
						continue
					case '`':
						if err := s.skipTemplate(); err != nil {
							return err
						}
						continue
					}
					s.pos++
				}
				continue
			}
		}
		s.pos++
	}
	return s.errorf("unterminated template literal")
}
