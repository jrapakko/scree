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

// MaxNameBytes is the longest name kept; longer ones are dropped.
const MaxNameBytes = 128

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
// lowercased, at most MaxNameBytes each, sorted and without duplicates.
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

// normalize lowercases names and drops empty, too short, too long and
// stoplisted ones, and duplicates.
func normalize(names []string) []string {
	out := names[:0]
	for _, n := range names {
		n = strings.ToLower(strings.TrimSpace(n))
		if len(n) < minNameBytes || len(n) > MaxNameBytes || stop[n] || !printable(n) {
			continue
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func printable(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] == 0x7f {
			return false
		}
	}
	return true
}

// stop lists common English words that are also short package names, such
// as Go's x/sync or a dependency called "common". A name in it is never
// vocabulary, so plain questions using the word stay semantic.
var stop = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`
		sync text sys net crypto time encoding errors io os log http json
		test tests testing util utils core common api client server context
		config types math path image mod tools term exp run new data base
		lib internal cmd main app web file files string strings sort bytes
		hash html xml yaml url color build cache event events flag check
		pretty mock rich click black`) {
		stop[w] = true
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

// lines calls fn for each line of src, without its line ending. Lines over
// maxLine bytes (minified or generated code) are skipped.
func lines(src []byte, fn func(line string)) {
	for len(src) > 0 {
		l := src
		if i := bytes.IndexByte(src, '\n'); i >= 0 {
			l, src = src[:i], src[i+1:]
		} else {
			src = nil
		}
		if len(l) <= maxLine {
			fn(strings.TrimSuffix(string(l), "\r"))
		}
	}
}

// maxLine is the longest line read.
const maxLine = 4096

// pep508Name is the distribution name at the start of a requirement.
var pep508Name = regexp.MustCompile(`^\s*([A-Za-z0-9][A-Za-z0-9._-]*)`)

// requirements reads package names from a pip requirements file: the name
// before any extras, version specifier, marker or URL. Options (-r, -e,
// --hash), URLs and paths are skipped.
func requirements(src []byte) []string {
	var out []string
	lines(src, func(l string) {
		if i := strings.Index(l, " #"); i >= 0 {
			l = l[:i]
		}
		l = strings.TrimSpace(l)
		if l == "" || l[0] == '#' || l[0] == '-' || l[0] == '.' || l[0] == '/' ||
			(strings.Contains(l, "://") && !strings.Contains(l, "@")) {
			return
		}
		if m := pep508Name.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		}
	})
	return out
}

// tomlTable returns the table name of a [table] or [[array]] header line,
// and whether the line is one.
func tomlTable(l string) (string, bool) {
	l = strings.TrimSpace(l)
	if i := strings.Index(l, "#"); i > 0 && !strings.ContainsAny(l[:i], `"'`) {
		l = strings.TrimSpace(l[:i])
	}
	if !strings.HasPrefix(l, "[") || !strings.HasSuffix(l, "]") {
		return "", false
	}
	l = strings.Trim(l, "[]")
	parts := strings.Split(l, ".")
	for i, p := range parts {
		parts[i] = strings.Trim(strings.TrimSpace(p), `"'`)
	}
	return strings.Join(parts, "."), true
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
	lines(src, func(l string) {
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
	lines(src, func(l string) {
		if t, ok := tomlTable(l); ok {
			table = t
			if name, ok := strings.CutPrefix(t, "dependencies."); ok && !strings.Contains(name, ".") {
				out = append(out, name)
			}
			return
		}
		if table != "dependencies" {
			return
		}
		if key, _, ok := tomlKey(l); ok && !strings.ContainsAny(key, " .[{") {
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
	lines(src, func(l string) {
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

var (
	pyDef    = regexp.MustCompile(`^\s*(?:async\s+)?def\s+([A-Za-z_]\w*)`)
	pyClass  = regexp.MustCompile(`^\s*class\s+([A-Za-z_]\w*)`)
	pyImport = regexp.MustCompile(`^\s*import\s+(.+)`)
	pyFrom   = regexp.MustCompile(`^\s*from\s+([\w.]+)\s+import\s+(.+)`)
	word     = regexp.MustCompile(`[A-Za-z_$][\w$]*`)
)

// python reads names from def, class, import and from ... import lines,
// including a parenthesized import list over several lines.
func python(src []byte) []string {
	var out []string
	inParen := false
	lines(src, func(l string) {
		if inParen {
			body, _, closed := strings.Cut(stripHash(l), ")")
			out = append(out, word.FindAllString(body, -1)...)
			inParen = !closed
			return
		}
		if m := pyDef.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		} else if m := pyClass.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
		} else if m := pyFrom.FindStringSubmatch(l); m != nil {
			out = append(out, strings.Split(m[1], ".")...)
			rest := stripHash(m[2])
			if strings.HasPrefix(strings.TrimSpace(rest), "(") {
				body, _, closed := strings.Cut(rest, ")")
				inParen = !closed
				rest = body
			}
			out = append(out, word.FindAllString(rest, -1)...)
		} else if m := pyImport.FindStringSubmatch(l); m != nil {
			out = append(out, word.FindAllString(stripHash(m[1]), -1)...)
		}
	})
	return out
}

func stripHash(s string) string {
	if i := strings.IndexByte(s, '#'); i >= 0 {
		return s[:i]
	}
	return s
}

var (
	jsDecl = regexp.MustCompile(`^\s*(?:export\s+)?(?:default\s+)?(?:declare\s+)?(?:abstract\s+)?(?:async\s+)?` +
		`(?:function\s*\*?|class|interface|type|enum|const|let|var)\s+([A-Za-z_$][\w$]*)`)
	jsDestructure = regexp.MustCompile(`^\s*(?:export\s+)?(?:const|let|var)\s*[{\[]([^}\]]*)[}\]]`)
	jsImport      = regexp.MustCompile(`^\s*(?:import|export)\b(.*)`)
	jsString      = regexp.MustCompile("\"[^\"]*\"|'[^']*'|`[^`]*`")
)

// javascript reads names from JavaScript and TypeScript declarations
// (function, class, interface, type, enum, const, let, var, with or without
// export) and from import and export statements, including a braced list
// over several lines. Module specifiers (the quoted paths) are skipped.
func javascript(src []byte) []string {
	var out []string
	inBrace := false
	lines(src, func(l string) {
		if c := strings.Index(l, "//"); c >= 0 {
			l = l[:c]
		}
		if inBrace {
			body, _, closed := strings.Cut(l, "}")
			out = append(out, word.FindAllString(jsString.ReplaceAllString(body, ""), -1)...)
			inBrace = !closed
			return
		}
		if m := jsDecl.FindStringSubmatch(l); m != nil {
			out = append(out, m[1])
			return
		}
		if m := jsDestructure.FindStringSubmatch(l); m != nil {
			out = append(out, word.FindAllString(m[1], -1)...)
			return
		}
		if m := jsImport.FindStringSubmatch(l); m != nil {
			rest := m[1]
			if i := strings.Index(rest, " from "); i >= 0 {
				rest = rest[:i]
			} else if i := strings.IndexAny(rest, `="'`+"`("); i >= 0 {
				rest = rest[:i]
			}
			if strings.Contains(rest, "{") && !strings.Contains(rest, "}") {
				inBrace = true
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
	goImp   = regexp.MustCompile(`^\s*(?:import\s+)?([A-Za-z_.]\w*\s+)?"([^"]+)"`)
)

// golang reads names from top-level Go declarations: func (methods too),
// type, const and var, single or grouped, and import paths and aliases.
func golang(src []byte) []string {
	var out []string
	group := ""
	lines(src, func(l string) {
		if group != "" {
			if strings.HasPrefix(l, ")") {
				group = ""
				return
			}
			if c := strings.Index(l, "//"); c >= 0 {
				l = l[:c]
			}
			if group == "import" {
				out = append(out, goImport(l)...)
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
		} else if strings.HasPrefix(l, "import ") {
			out = append(out, goImport(l)...)
		}
	})
	return out
}

// goImport returns an import spec's alias and the segments of its path.
func goImport(l string) []string {
	m := goImp.FindStringSubmatch(l)
	if m == nil {
		return nil
	}
	out := strings.Split(m[2], "/")
	if a := strings.TrimSpace(m[1]); a != "" {
		out = append(out, a)
	}
	return out
}
