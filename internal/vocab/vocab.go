// Package vocab collects a repo's code vocabulary: the names auto search
// mode treats as identifiers. It reads dependency names from manifests
// (requirements*.txt, pyproject.toml, package.json, go.mod, Cargo.toml) and
// mixed-case names (a lowercase letter followed by an uppercase one, such
// as PageFetcher or useState) from import statements and declarations in
// Python, JavaScript, TypeScript and Go source. Only the names are kept,
// lowercased; no line of code is returned, stored or embedded.
package vocab

import (
	"bytes"
	"encoding/json"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Version names the extraction rules. An index whose names were read under
// another version reads every file again; bump it whenever a change to this
// package would give different names for the same file.
const Version = "4"

// MaxNameBytes is the longest name kept; longer ones are dropped.
const MaxNameBytes = 128

// MaxFileNames caps the names kept from one file: the first ones in sorted
// order. A generated file with more is mostly noise.
const MaxFileNames = 2000

// MaxNames caps the names one index keeps across all files, about 5 MB of
// names at most (see index.Sync).
const MaxNames = 100000

// minNameBytes is the shortest bare name kept: two-letter names such as
// "go" or "ms" are too often ordinary words.
const minNameBytes = 3

// Kind says how a file is read.
type Kind int

const (
	None Kind = iota
	Requirements
	PyProject
	PackageJSON
	GoMod
	Cargo
	Python
	JS
	Go
)

// vendored folder names: code under them is a copy of someone else's and
// contributes nothing, even when git tracks it.
var vendored = []string{"vendor", "node_modules", "third_party", "site-packages", "bower_components"}

// KindOf returns how the file rel (a slash-separated path) is read, or None
// for a file that contributes nothing, such as one under a vendored folder.
func KindOf(rel string) Kind {
	segs := strings.Split(rel, "/")
	for _, s := range segs[:len(segs)-1] {
		if slices.Contains(vendored, s) {
			return None
		}
	}
	base := segs[len(segs)-1]
	switch base {
	case "pyproject.toml":
		return PyProject
	case "package.json":
		return PackageJSON
	case "go.mod":
		return GoMod
	case "Cargo.toml":
		return Cargo
	}
	if strings.HasPrefix(base, "requirements") && strings.HasSuffix(base, ".txt") {
		return Requirements
	}
	switch path.Ext(base) {
	case ".py", ".pyi":
		return Python
	case ".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts":
		if strings.HasSuffix(base, ".min.js") {
			return None
		}
		return JS
	case ".go":
		return Go
	}
	return None
}

// IsSource reports whether rel is a file the vocabulary reads.
func IsSource(rel string) bool { return KindOf(rel) != None }

// Extract returns the vocabulary names in src, read as the file rel:
// lowercased, printable ASCII, at most MaxNameBytes each, sorted, without
// duplicates and at most MaxFileNames of them. Each is a copy, so none
// keeps src alive.
func Extract(rel string, src []byte) []string {
	var names []string
	switch KindOf(rel) {
	case Requirements:
		names = requirements(src)
	case PyProject:
		names = pyproject(src)
	case PackageJSON:
		names = packageJSON(src)
	case GoMod:
		names = goMod(src)
	case Cargo:
		names = cargo(src)
	case Python:
		names = mixed(python(src))
	case JS:
		names = mixed(javascript(src))
	case Go:
		names = mixed(golang(src))
	}
	return normalize(names)
}

// normalize lowercases names and drops empty, too short, too long,
// non-ASCII and stoplisted ones, and duplicates. It keeps the first
// MaxFileNames, each copied out of the source text.
func normalize(names []string) []string {
	out := names[:0]
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if len(n) < minNameBytes || len(n) > MaxNameBytes || !printable(n) || stop[n] {
			continue
		}
		out = append(out, n)
	}
	slices.Sort(out)
	out = slices.Compact(out)
	if len(out) > MaxFileNames {
		out = out[:MaxFileNames]
	}
	for i, n := range out {
		out[i] = strings.Clone(n)
	}
	return out
}

// printable reports whether s is printable ASCII without spaces (0x21 to
// 0x7e), so no control, bidi or invalid UTF-8 byte reaches the index or
// doctor's output.
func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// stop lists words that are never vocabulary, so plain questions using them
// stay semantic: English function and question words, common words that are
// also package names (Go's x/sync, npm's got or marked, Python's requests),
// and plain words that code writes as mixed case (setUp, fileName). Each
// list is sorted.
var stop = map[string]bool{}

func init() {
	for _, list := range []string{
		// English function, question and common verbs.
		`about above after again all also and any are because been before
		being below between both but can cannot could did does doing done
		down during each else even every few for from get gets had has have
		having her here hers him his how into its just let like make many
		may might more most much must need not now off once one only other
		our out over own per put same set shall she should show some such
		than that the their them then there these they this those through
		too two under until upon use used uses using very via want was way
		were what when where which while who whom why will with work works
		would yes yet you your yours`,
		// Common words that are also package names.
		`abc ansi api app args array assert async attrs auth base black
		build bytes cache chart check cli click client clone cmd color
		colors commander common compress config context cookie cookies core
		cron crypto csv dash data date debug decimal deep defaults diff
		docker email encoding enum errors event events exp express extend
		fetch file files filter flag format forms futures glob got graph
		hash history html http image ini internal invariant json lib log
		logger logging main markdown marked math memo merge mime mock mod
		moment net next node open optional parse parser path pretty
		process progress promise prompt queue random regex request requests
		retry rich router run sdk server sharp shell signal six slug sort
		sql storage stream string strings styled sync sys table tar temp
		term test testing tests text thread time timer toml tools tracing
		types typing url util utils validator warning warnings web wrap xml
		yaml`,
		// Plain words written as mixed case: setUp, fileName, timeOut.
		`backend background blacklist callback checkbox checkout checkpoint
		cleanup dashboard database datetime download dropdown endpoint
		feedback filename filepath framework frontend fullscreen healthcheck
		homepage hostname hotfix keyboard keyword keywords layout lifecycle
		login logout lookup markup metadata namespace network newline
		notebook offline online overflow override overview password pathname
		placeholder playground popup readme readonly runtime screenshot
		setup shutdown sidebar signin signout signup snapshot startup
		stylesheet subdomain teardown textbox timeout timestamp toolbar
		upload username viewport webhook webpage website whitelist
		whitespace wildcard workflow workload workspace`,
	} {
		for _, w := range strings.Fields(list) {
			stop[w] = true
		}
	}
}

// mixedCase matches a lowercase ASCII letter followed by an uppercase one.
var mixedCase = regexp.MustCompile(`[a-z][A-Z]`)

// mixed keeps the names with a lowercase-then-uppercase transition.
func mixed(names []string) []string {
	out := names[:0]
	for _, n := range names {
		if len(n) <= MaxNameBytes && mixedCase.MatchString(n) {
			out = append(out, n)
		}
	}
	return out
}

// lines calls fn for each line of src, without its line ending. Every line
// goes through fn, so a scanner's string, comment and list state stays in
// step, but the names fn appends to *out for a line over maxLine bytes
// (minified or generated code) are dropped.
func lines(src []byte, out *[]string, fn func(line string)) {
	for len(src) > 0 {
		l := src
		if i := bytes.IndexByte(src, '\n'); i >= 0 {
			l, src = src[:i], src[i+1:]
		} else {
			src = nil
		}
		n := len(*out)
		fn(strings.TrimSuffix(string(l), "\r"))
		if len(l) > maxLine {
			*out = (*out)[:n]
		}
	}
}

// maxLine is the longest line whose names are kept.
const maxLine = 4096

// pep508Name is the distribution name at the start of a requirement. It
// must be followed by the end, whitespace (as before "name @ url") or a
// character that starts extras, a version specifier or a marker, so
// "user:token@host/path" and "git+https://..." give no name.
var pep508Name = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)(?:$|[\s\[;(,<>=!~])`)

// requirements reads package names from a pip requirements file: the name
// before any extras, version specifier, marker or " @ url". Options (-r,
// -e, --hash), URLs and paths are skipped.
func requirements(src []byte) []string {
	var out []string
	lines(src, &out, func(l string) {
		if i := strings.Index(l, " #"); i >= 0 {
			l = l[:i]
		}
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' || l[0] == '-' || l[0] == '.' || l[0] == '/' {
			return
		}
		if m := pep508Name.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		}
	})
	return out
}

// tomlKeyPart is one bare or quoted part of a TOML key.
const tomlKeyPart = `(?:[A-Za-z0-9_-]+|"(?:[^"\\]|\\.)*"|'[^']*')`

var (
	tomlHeader = regexp.MustCompile(`^\s*(\[\[?)\s*(` + tomlKeyPart + `(?:\s*\.\s*` + tomlKeyPart + `)*)\s*(\]\]?)\s*(?:#.*)?$`)
	tomlPart   = regexp.MustCompile(tomlKeyPart)
)

// tomlTable returns the table name of a [table] or [[array]] header line,
// its dotted key with quotes dropped, and whether the line is one. Only a
// whole header counts, so a line such as ["E", "F"] inside a multi-line
// array is not one.
func tomlTable(l string) (string, bool) {
	m := tomlHeader.FindStringSubmatch(l)
	if m == nil || len(m[1]) != len(m[3]) {
		return "", false
	}
	parts := tomlPart.FindAllString(m[2], -1)
	for i, p := range parts {
		parts[i] = strings.Trim(p, `"'`)
	}
	return strings.Join(parts, "."), true
}

// bracketDepth returns the [ minus ] outside quotes and comments in l: how
// far a line opens (or closes) a multi-line array.
func bracketDepth(l string) int {
	d := 0
	quote := byte(0)
	l = stripComment(l)
	for i := 0; i < len(l); i++ {
		switch c := l[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			d++
		case c == ']':
			d--
		}
	}
	return d
}

// tomlKey returns the bare or quoted key of a "key = value" line, and the
// value text.
func tomlKey(l string) (key, val string, ok bool) {
	k, v, found := strings.Cut(l, "=")
	if !found {
		return "", "", false
	}
	k = strings.TrimSpace(k)
	if k == "" || strings.HasPrefix(k, "#") {
		return "", "", false
	}
	return strings.Trim(k, `"'`), strings.TrimSpace(v), true
}

// tomlStrings returns the quoted strings in s.
var tomlString = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|'([^']*)'`)

func tomlStrings(s string) []string {
	var out []string
	for _, m := range tomlString.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1]+m[2])
	}
	return out
}

// pyproject reads [project] dependencies (PEP 508 strings) and the keys of
// [tool.poetry.dependencies], except python. No other array counts, so a
// tool setting such as ruff's select = ["E", "F"] contributes nothing.
func pyproject(src []byte) []string {
	var out []string
	table := ""
	inDeps := false // inside a multi-line [project] dependencies array
	skip := 0       // depth inside another multi-line array
	lines(src, &out, func(l string) {
		if skip > 0 {
			skip = max(skip+bracketDepth(l), 0)
			return
		}
		if inDeps {
			body, closed := cutArray(stripComment(l))
			for _, s := range tomlStrings(body) {
				if m := pep508Name.FindStringSubmatch(s); m != nil {
					out = append(out, m[1])
				}
			}
			inDeps = !closed
			return
		}
		if t, ok := tomlTable(l); ok {
			table = t
			return
		}
		key, val, ok := tomlKey(l)
		if !ok {
			return
		}
		switch {
		case table == "project" && key == "dependencies":
			val = stripComment(val)
			if !strings.HasPrefix(val, "[") {
				return
			}
			body, closed := cutArray(val[1:])
			for _, s := range tomlStrings(body) {
				if m := pep508Name.FindStringSubmatch(s); m != nil {
					out = append(out, m[1])
				}
			}
			inDeps = !closed
		case table == "tool.poetry.dependencies" && key != "python":
			out = append(out, key)
			skip = max(bracketDepth(val), 0)
		default:
			skip = max(bracketDepth(val), 0)
		}
	})
	return out
}

// cutArray returns s up to the first "]" outside quotes, and whether there
// was one: the end of a TOML array whose strings may hold brackets, such as
// "sqlalchemy[asyncio]".
func cutArray(s string) (string, bool) {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ']':
			return s[:i], true
		}
	}
	return s, false
}

// stripComment drops a # comment outside quotes.
func stripComment(s string) string {
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#':
			return s[:i]
		}
	}
	return s
}

// cargo reads the keys of [dependencies] and the names of
// [dependencies.name] tables.
func cargo(src []byte) []string {
	var out []string
	table := ""
	skip := 0 // depth inside a multi-line array
	lines(src, &out, func(l string) {
		if skip > 0 {
			skip = max(skip+bracketDepth(l), 0)
			return
		}
		if t, ok := tomlTable(l); ok {
			table = t
			if name, ok := strings.CutPrefix(t, "dependencies."); ok && !strings.Contains(name, ".") {
				out = append(out, name)
			}
			return
		}
		key, val, ok := tomlKey(l)
		if !ok {
			return
		}
		skip = max(bracketDepth(val), 0)
		if table == "dependencies" && !strings.ContainsAny(key, " .[{") {
			out = append(out, key)
		}
	})
	return out
}

// packageJSON reads the keys of dependencies, devDependencies and
// peerDependencies.
func packageJSON(src []byte) []string {
	var pkg struct {
		Deps map[string]json.RawMessage `json:"dependencies"`
		Dev  map[string]json.RawMessage `json:"devDependencies"`
		Peer map[string]json.RawMessage `json:"peerDependencies"`
	}
	if json.Unmarshal(src, &pkg) != nil {
		return nil
	}
	var out []string
	for _, m := range []map[string]json.RawMessage{pkg.Deps, pkg.Dev, pkg.Peer} {
		for k := range m {
			out = append(out, k)
		}
	}
	return out
}

// goMod reads the module paths of go.mod's require directives (single and
// grouped), skipping // indirect ones, and adds each one's names (see
// goNames).
func goMod(src []byte) []string {
	var out []string
	inBlock := false
	lines(src, &out, func(l string) {
		l = strings.TrimSpace(l)
		if strings.HasSuffix(l, "// indirect") {
			return
		}
		if c := strings.Index(l, "//"); c >= 0 {
			l = strings.TrimSpace(l[:c])
		}
		var spec string
		switch {
		case inBlock && l == ")":
			inBlock = false
			return
		case inBlock:
			spec = l
		case l == "require (":
			inBlock = true
			return
		case strings.HasPrefix(l, "require "):
			spec = strings.TrimSpace(strings.TrimPrefix(l, "require "))
		default:
			return
		}
		if f := strings.Fields(spec); len(f) >= 1 {
			out = append(out, goNames(strings.Trim(f[0], `"`))...)
		}
	})
	return out
}

// majorSuffix matches a /vN or .vN major-version suffix.
var majorSuffix = regexp.MustCompile(`[/.]v[0-9]+$`)

// goNames returns the names a Go module path adds: the full path, its last
// segment without a /vN (or gopkg.in's .vN) suffix, and that segment
// without a go- prefix. For golang.org/x/ and other standard-library-style
// paths (a first element with no dot) only the full path is added, since
// their last segments are ordinary words like sync or text. Stoplisted
// segments are dropped later by normalize.
func goNames(mod string) []string {
	if mod == "" {
		return nil
	}
	out := []string{mod}
	first, _, _ := strings.Cut(mod, "/")
	if strings.HasPrefix(mod, "golang.org/x/") || !strings.Contains(first, ".") {
		return out
	}
	last := majorSuffix.ReplaceAllString(mod, "")
	last = last[strings.LastIndex(last, "/")+1:]
	if last == "" || last == first {
		return out
	}
	out = append(out, last)
	if t, ok := strings.CutPrefix(last, "go-"); ok {
		out = append(out, t)
	}
	return out
}

// maxListLines caps the lines of one parenthesized or braced import list;
// past it the list is taken as ended.
const maxListLines = 200

// pyItem is one dotted name with an optional alias, as in an import list.
const pyItem = `[\w.]+(?:\s+as\s+\w+)?`

var (
	pyDef    = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`)
	pyClass  = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`)
	pyImport = regexp.MustCompile(`^\s*import\s+(` + pyItem + `(?:\s*,\s*` + pyItem + `)*)\s*\\?\s*$`)
	pyFrom   = regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\s+(.*)$`)
	// pyNames is an import list, or a line of one: names with optional
	// aliases, comma-separated, maybe with a trailing comma or backslash.
	pyNames = regexp.MustCompile(`^\s*(?:` + pyItem + `(?:\s*,\s*` + pyItem + `)*)?\s*,?\s*\\?\s*$`)
	word    = regexp.MustCompile(`[A-Za-z_$][\w$]*`)
)

// python reads names from def, class, import and from ... import lines,
// including a parenthesized import list over several lines. Only names in
// name positions count: an import line is cut at the first ;, #, = or
// quote and must then be an import list, a list line that is not one ends
// the list, and lines inside a """ or ”' string are skipped.
func python(src []byte) []string {
	var out []string
	paren := 0   // lines left in a parenthesized import list, 0 outside one
	triple := "" // delimiter of the """ or ''' string the line is in
	lines(src, &out, func(l string) {
		if triple != "" {
			if strings.Contains(l, triple) {
				triple = ""
			}
			return
		}
		triple = pyTriple(l)
		if paren > 0 {
			paren--
			body, after, closed := strings.Cut(stripHash(l), ")")
			if pyNames.MatchString(body) && strings.TrimSpace(after) == "" {
				out = append(out, word.FindAllString(body, -1)...)
				if closed {
					paren = 0
				}
				return
			}
			paren = 0 // not a list line: the list has ended
		}
		if m := pyDef.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
			return
		}
		if m := pyClass.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
			return
		}
		code := pyCut(l)
		if m := pyFrom.FindStringSubmatch(code); m != nil {
			rest := strings.TrimSpace(m[2])
			if r, ok := strings.CutPrefix(rest, "("); ok {
				body, after, closed := strings.Cut(r, ")")
				if !pyNames.MatchString(body) || strings.TrimSpace(after) != "" {
					return
				}
				if !closed {
					paren = maxListLines
				}
				rest = body
			} else if rest != "*" && !pyNames.MatchString(rest) {
				return
			}
			out = append(out, strings.Split(m[1], ".")...)
			out = append(out, word.FindAllString(rest, -1)...)
		} else if m := pyImport.FindStringSubmatch(code); m != nil {
			out = append(out, word.FindAllString(m[1], -1)...)
		}
	})
	return out
}

// pyTriple returns the delimiter of a """ or ”' string that l opens and
// leaves open, or "". A comment line opens none, and a line that opens and
// closes one (an odd count is open) leaves none.
func pyTriple(l string) string {
	if strings.HasPrefix(strings.TrimSpace(l), "#") {
		return ""
	}
	d, i := `"""`, strings.Index(l, `"""`)
	if j := strings.Index(l, "'''"); j >= 0 && (i < 0 || j < i) {
		d, i = "'''", j
	}
	if i < 0 || strings.Count(l[i:], d)%2 == 0 {
		return ""
	}
	return d
}

// pyCut cuts l at the first ;, #, = or quote: where an import statement
// ends, or where something that is not one starts.
func pyCut(l string) string {
	if i := strings.IndexAny(l, `;#='"`); i >= 0 {
		return l[:i]
	}
	return l
}

func stripHash(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i]
	}
	return s
}

// cState is where a C-like scanner (see strip) is at the end of a line:
// inside a /* */ comment or a backtick string.
type cState struct{ comment, tick bool }

// strip returns l without comments and with the contents of strings
// dropped, quotes kept, so "a" reads as "". Block comments and backtick
// strings may span lines; quoted strings end at the line's end. Backslash
// escapes apply inside quotes, and inside backticks unless raw (Go).
func (s *cState) strip(l string, raw bool) string {
	var b strings.Builder
	quote := byte(0)
	if s.tick {
		quote = '`'
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		switch {
		case s.comment:
			if c == '*' && i+1 < len(l) && l[i+1] == '/' {
				s.comment = false
				i++
				b.WriteByte(' ')
			}
		case quote != 0:
			if c == '\\' && !(raw && quote == '`') {
				i++
			} else if c == quote {
				quote = 0
				b.WriteByte(c)
			}
		case c == '/' && i+1 < len(l) && l[i+1] == '/':
			s.tick = false
			return b.String()
		case c == '/' && i+1 < len(l) && l[i+1] == '*':
			s.comment = true
			i++
		case c == '"' || c == '\'' || c == '`':
			quote = c
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	s.tick = quote == '`'
	return b.String()
}

// jsSpec is one name of an import or export list, as in "type A as B".
const jsSpec = `(?:type\s+)?[\w$]+(?:\s+as\s+[\w$]+)?`

var (
	jsDecl = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?(?:async\s+)?` +
		`(?:function\s*\*?|class|interface|type|enum|const|let|var)\s+([A-Za-z_$][\w$]*)`)
	jsDestructure = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s*[{\[]([^}\]]*)[}\]]`)
	jsImport      = regexp.MustCompile(`^\s*(import|export)\b(.*)`)
	// jsExport is how an export statement that is not a declaration starts:
	// a braced list, *, or type and a braced list. A bare name after export
	// is not a name position.
	jsExport = regexp.MustCompile(`^\s*(?:\{|\*|type\s*\{)`)
	jsFrom   = regexp.MustCompile(`\s+from\s*$`)
	// jsSpecs is a list of jsSpec, or a line of one.
	jsSpecs = regexp.MustCompile(`^\s*(?:` + jsSpec + `(?:\s*,\s*` + jsSpec + `)*)?\s*,?\s*$`)
	// jsClause is what may follow import or export before the module
	// specifier: a default name (for import; see jsExport), * as name, and
	// a braced list, opened or closed on the line.
	jsClause = regexp.MustCompile(`^\s*(?:type\s+)?(?:[\w$]+\s*(?:,\s*)?)?(?:\*(?:\s+as\s+[\w$]+)?\s*)?` +
		`(?:\{([^{}]*)(\})?\s*)?$`)
)

// javascript reads names from JavaScript and TypeScript declarations
// (function, class, interface, type, enum, const, let, var, with or without
// export) and from import and export statements, including a braced list
// over several lines. Comments and the contents of strings are dropped
// first, and a line is cut at its first ;. Only declared names count, not
// initializers; an import or export must have the shape of one, and a list
// line that is not a list of names ends the list.
func javascript(src []byte) []string {
	var out []string
	var cs cState
	brace := 0 // lines left in a braced import or export list, 0 outside one
	lines(src, &out, func(l string) {
		l = cs.strip(l, false)
		if i := strings.IndexByte(l, ';'); i >= 0 {
			l = l[:i]
		}
		if brace > 0 {
			brace--
			body, _, closed := strings.Cut(l, "}")
			if jsSpecs.MatchString(body) {
				out = append(out, word.FindAllString(body, -1)...)
				if closed {
					brace = 0
				}
				return
			}
			brace = 0 // not a list line: the list has ended
		}
		if m := jsDecl.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
			return
		}
		if m := jsDestructure.FindStringSubmatch(l); m != nil {
			// The bound names only: a default value after = and a property
			// name before : are not declared.
			for _, el := range strings.Split(m[1], ",") {
				el, _, _ = strings.Cut(el, "=")
				if _, after, ok := strings.Cut(el, ":"); ok {
					el = after
				}
				out = append(out, word.FindAllString(el, -1)...)
			}
			return
		}
		if m := jsImport.FindStringSubmatch(l); m != nil {
			rest := m[2]
			if m[1] == "export" && !jsExport.MatchString(rest) {
				return
			}
			if i := strings.IndexAny(rest, `="'`+"`("); i >= 0 {
				rest = rest[:i]
			}
			rest = jsFrom.ReplaceAllString(rest, "")
			c := jsClause.FindStringSubmatch(rest)
			if c == nil || !jsSpecs.MatchString(c[1]) {
				return
			}
			if strings.Contains(rest, "{") && c[2] == "" {
				brace = maxListLines
			}
			out = append(out, word.FindAllString(rest, -1)...)
		}
	})
	return out
}

var (
	goFunc  = regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`)
	goDecl  = regexp.MustCompile(`^(type|const|var)\s+([A-Za-z_]\w*)`)
	goGroup = regexp.MustCompile(`^(type|const|var|import)\s*\(\s*$`)
	goSpec  = regexp.MustCompile(`^\s*([A-Za-z_]\w*(?:\s*,\s*[A-Za-z_]\w*)*)`)
	goImp   = regexp.MustCompile(`^\s*(?:import\s+)?(?:([A-Za-z_.]\w*)\s+)?"([\w./~+-]+)"\s*(?://.*)?$`)
)

// golang reads names from top-level Go declarations: func (methods too),
// type, const and var, single or grouped, and import paths and aliases.
// Comments, the contents of strings and raw strings over several lines are
// dropped first, so only declared names count, never values; an import
// group line that is not an import spec ends the group.
func golang(src []byte) []string {
	var out []string
	var cs cState
	group := ""
	lines(src, &out, func(raw string) {
		code := !cs.comment && !cs.tick // the line starts in code
		l := cs.strip(raw, true)
		if group != "" {
			if strings.HasPrefix(l, ")") {
				group = ""
				return
			}
			if group == "import" {
				if !code || strings.TrimSpace(l) == "" {
					return
				}
				if names := goImport(raw); names != nil {
					out = append(out, names...)
				} else {
					group = "" // not an import spec: the group has ended
				}
				return
			}
			// One spec per line at the group's indent: names before the type
			// or "=". Deeper lines are struct fields or values.
			if strings.HasPrefix(l, "\t\t") || strings.HasPrefix(l, "        ") {
				return
			}
			if m := goSpec.FindStringSubmatch(l); m != nil {
				out = append(out, word.FindAllString(m[1], -1)...)
			}
			return
		}
		if m := goGroup.FindStringSubmatch(l); m != nil {
			group = m[1]
			return
		}
		if m := goFunc.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		} else if m := goDecl.FindStringSubmatch(l); m != nil {
			out = append(out, m[2])
		} else if code && strings.HasPrefix(l, "import ") {
			out = append(out, goImport(raw)...)
		}
	})
	return out
}

// goImport returns an import spec's alias and the segments of its path,
// or nil if l is not exactly one spec.
func goImport(l string) []string {
	m := goImp.FindStringSubmatch(l)
	if m == nil {
		return nil
	}
	out := strings.Split(m[2], "/")
	if m[1] != "" {
		out = append(out, m[1])
	}
	return out
}
