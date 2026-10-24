package vocab

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

func check(t *testing.T, rel, src string, want, notWant []string) {
	t.Helper()
	got := Extract(rel, []byte(src))
	for _, w := range want {
		if !slices.Contains(got, w) {
			t.Errorf("%s: %q missing from %q", rel, w, got)
		}
	}
	for _, w := range notWant {
		if slices.Contains(got, w) {
			t.Errorf("%s: %q should not be in %q", rel, w, got)
		}
	}
}

func TestRequirements(t *testing.T) {
	check(t, "requirements-dev.txt", `# tools
psycopg[binary,pool]>=3.1  # the driver
Flask==2.3.0
requests ; python_version >= "3.8"
pydantic~=2.0
-r base.txt
-e git+https://example.com/x.git#egg=editable
https://example.com/pkg.tar.gz
./local/path
torch @ https://example.com/torch.whl
uvicorn
`, []string{"psycopg", "flask", "pydantic", "torch", "uvicorn"},
		[]string{"requests", "binary", "pool", "base.txt", "editable", "https", "local", "2.3.0"})
	if KindOf("requirements/dev.txt") != None || KindOf("sub/requirements.txt") != Requirements {
		t.Error("requirements file kinds")
	}
}

func TestPyProject(t *testing.T) {
	check(t, "pyproject.toml", `[project]
name = "demo"
dependencies = [
    "httpx>=0.27",  # client
    'sqlalchemy[asyncio]',
    "anyio ; python_version<'3.11'",
]
optional-dependencies = { dev = ["pytest"] }

[tool.ruff.lint]
select = ["E", "F", "UP", "B", "SIM"]
ignore = ["E501"]

[tool.poetry.dependencies]
python = "^3.11"
fastapi = "^0.110"
"uvloop" = { version = "*", optional = true }

[tool.black]
target-version = ["py311"]
`, []string{"httpx", "sqlalchemy", "anyio", "fastapi", "uvloop"},
		[]string{"demo", "sim", "e501", "python", "py311", "asyncio", "pytest", "target-version", "select"})
	// A one-line array.
	check(t, "a/pyproject.toml", "[project]\ndependencies = [\"rapidfuzz\", \"orjson>=3\"]\n[tool.x]\nselect = [\"ALL\"]\n",
		[]string{"rapidfuzz", "orjson"}, []string{"all"})
}

func TestPackageJSON(t *testing.T) {
	check(t, "web/package.json", `{
  "name": "site",
  "scripts": {"buildSite": "vite build"},
  "dependencies": {"react": "^18", "@tanstack/react-query": "^5"},
  "devDependencies": {"vitest": "^1"},
  "peerDependencies": {"react-dom": "^18"},
  "keywords": ["frontendStuff"]
}`, []string{"react", "@tanstack/react-query", "vitest", "react-dom"}, []string{"site", "buildsite", "frontendstuff"})
	if got := Extract("package.json", []byte("{not json")); len(got) != 0 {
		t.Errorf("bad JSON: %q", got)
	}
}

func TestGoMod(t *testing.T) {
	check(t, "go.mod", `module github.com/me/app

go 1.24

require github.com/spf13/cobra v1.8.0

require (
	github.com/jackc/pgx/v5 v5.5.0
	github.com/mattn/go-sqlite3 v1.14.0
	golang.org/x/sync v0.7.0
	golang.org/x/text v0.14.0 // indirect
	github.com/acme/sync v1.0.0
	github.com/acme/go-json v1.0.0
	gopkg.in/yaml.v3 v3.0.1
	github.com/kr/pretty v0.3.1 // indirect
)

replace github.com/x/y => ../y
`, []string{
		"github.com/spf13/cobra", "cobra",
		"github.com/jackc/pgx/v5", "pgx",
		"github.com/mattn/go-sqlite3", "go-sqlite3", "sqlite3",
		"golang.org/x/sync",
		"github.com/acme/sync", "github.com/acme/go-json", "go-json",
		"gopkg.in/yaml.v3",
	}, []string{
		"v5", "sync", "text", "golang.org/x/text", "json", "yaml", "pretty",
		"github.com/me/app", "app", "y", "github.com/x/y",
	})
}

func TestGoNamesStdlibStyle(t *testing.T) {
	for mod, want := range map[string][]string{
		"golang.org/x/sync":         {"golang.org/x/sync"},
		"golang.org/x/net/v2":       {"golang.org/x/net/v2"},
		"example/internal/thing":    {"example/internal/thing"},
		"github.com/foo/bar/v2":     {"github.com/foo/bar/v2", "bar"},
		"github.com/foo/go-bar":     {"github.com/foo/go-bar", "go-bar", "bar"},
		"github.com/ncruces/go-sql": {"github.com/ncruces/go-sql", "go-sql", "sql"},
	} {
		if got := goNames(mod); !slices.Equal(got, want) {
			t.Errorf("goNames(%q) = %q, want %q", mod, got, want)
		}
	}
}

func TestCargo(t *testing.T) {
	check(t, "Cargo.toml", `[package]
name = "tool"
edition = "2021"

[dependencies]
serde = { version = "1", features = ["derive"] }
tokio = "1"
"reqwest" = "0.12"

[dependencies.clap]
version = "4"

[dev-dependencies]
proptest = "1"

[features]
fancyMode = []
`, []string{"serde", "tokio", "reqwest", "clap"}, []string{"tool", "derive", "proptest", "fancymode", "version", "edition"})
}

func TestStoplist(t *testing.T) {
	check(t, "package.json", `{"dependencies": {"common": "1", "core": "1", "lodash": "4", "io": "1", "api": "1"}}`,
		[]string{"lodash"}, []string{"common", "core", "io", "api"})
	if n := len(stop); n < 250 || n > 500 {
		t.Errorf("stoplist has %d words", n)
	}
	// Function and question words, words that are also packages, and plain
	// words written as mixed case.
	check(t, "package.json", `{"dependencies": {"requests": "1", "markdown": "1", "next": "1", "debug": "1",
		"async": "1", "moment": "1", "got": "1", "marked": "1", "sharp": "1", "express": "1", "tracing": "1",
		"futures": "1", "storage": "1", "cli": "1", "sdk": "1", "docker": "1", "config": "1", "utils": "1",
		"how": "1", "what": "1", "where": "1", "the": "1", "and": "1", "for": "1", "with": "1", "from": "1",
		"does": "1", "zustand": "1"}}`, []string{"zustand"},
		[]string{"requests", "markdown", "next", "debug", "async", "moment", "got", "marked", "sharp", "express",
			"tracing", "futures", "storage", "cli", "sdk", "docker", "config", "utils",
			"how", "what", "where", "the", "and", "for", "with", "from", "does"})
	check(t, "a.py", `def setUp(self): pass
def tearDown(self): pass
def logIn(): pass
def logOut(): pass
def signUp(): pass
def signIn(): pass
class FileName: pass
class TimeOut: pass
class MetaData: pass
class DataBase: pass
def hostName(): pass
def userName(): pass
def passWord(): pass
def keyWord(): pass
def fetchPage(): pass
`, []string{"fetchpage"}, []string{"setup", "teardown", "login", "logout", "signup", "signin", "filename",
		"timeout", "metadata", "database", "hostname", "username", "password", "keyword"})
}

// TestNormalizeASCII: only printable ASCII names are kept, so no C1 control,
// bidi override or invalid UTF-8 reaches the index.
func TestNormalizeASCII(t *testing.T) {
	got := normalize([]string{"okName", "bad\u0085Name", "evil\u202eName", "inv\xffalid", "caf\u00e9Name", "tab\tName", "fineToo"})
	if want := []string{"finetoo", "okname"}; !slices.Equal(got, want) {
		t.Errorf("normalize = %q, want %q", got, want)
	}
}

// TestMaxFileNames: a file keeps at most MaxFileNames names, the first in
// sorted order.
func TestMaxFileNames(t *testing.T) {
	var b strings.Builder
	for i := range MaxFileNames + 50 {
		fmt.Fprintf(&b, "func fnName%05d() {}\n", i)
	}
	got := Extract("gen.go", []byte(b.String()))
	if len(got) != MaxFileNames || got[0] != "fnname00000" || got[len(got)-1] != fmt.Sprintf("fnname%05d", MaxFileNames-1) {
		t.Errorf("got %d names, %q .. %q", len(got), got[0], got[len(got)-1])
	}
}

func TestTomlTable(t *testing.T) {
	for l, want := range map[string]string{
		"[project]":                      "project",
		"  [tool.poetry.dependencies]  ": "tool.poetry.dependencies",
		"[[bin]]":                        "bin",
		`[dependencies."my.crate"]`:      "dependencies.my.crate",
		"[ tool . ruff ] # lint":         "tool.ruff",
		`['quoted key']`:                 "quoted key",
	} {
		if got, ok := tomlTable(l); !ok || got != want {
			t.Errorf("tomlTable(%q) = %q, %v, want %q", l, got, ok, want)
		}
	}
	for _, l := range []string{`["E", "F"]`, `[1, 2],`, `["x"],`, "[a]]", "[[a]", "[]", "[a b]", `x = ["y"]`} {
		if got, ok := tomlTable(l); ok {
			t.Errorf("tomlTable(%q) = %q, want no header", l, got)
		}
	}
}

// TestPyProjectArrays: a line inside another multi-line array that looks
// like a header does not end [project] or a dependency table.
func TestPyProjectArrays(t *testing.T) {
	check(t, "pyproject.toml", `[project]
classifiers = [
    "Programming Language :: Python",
]
keywords = [
    ["nested"],
    ["dependencies"]
]
dependencies = ["rapidfuzz"]

[tool.poetry.dependencies]
extrasThing = { version = "1", extras = [
    "a",
] }
orjson = "^3"
`, []string{"rapidfuzz", "orjson"}, []string{"nested", "a"})
	check(t, "Cargo.toml", `[dependencies]
serde = { version = "1", features = [
    ["derive"]
] }
tokio = "1"
`, []string{"serde", "tokio"}, []string{"derive"})
}

func TestPython(t *testing.T) {
	check(t, "pkg/engine.py", `import numpy as np, osPathHelper
from fastAPI.routing import APIRouter, pageFetcher as pFetcher
from .models import (
    UserModel,  # the user
    orderItem,
)
class PageFetcher(BaseFetcher):
    async def fetchAll(self, query):
        localValue = makeThing()  # not a declaration
        return localValue
def parse_args(): pass
x = "class FakeName: pass"
`, []string{"ospathhelper", "fastapi", "pagefetcher", "pfetcher", "usermodel", "orderitem", "fetchall"},
		[]string{"numpy", "np", "apirouter", "baseengine", "localvalue", "makething", "parse_args", "fakename", "user"})
}

func TestJS(t *testing.T) {
	check(t, "src/App.tsx", `import React, { useState, useEffect as onMount } from 'react'
import * as fileUtils from "./fileUtils"
import {
  QueryClient,
  queryOptions,
} from '@tanstack/react-query'
export default function AppShell() {
  const [count, setCount] = useState(0)
  const { dataRows, isLoading } = useQuery()
  let tmpValue = helperCall(otherName)
}
export const routeTable = {}
export { routeTable as routesMap }
export interface UserProps { userName: string }
export type RowKind = 'a' | 'b'
class DataStore extends BaseStore {}
var oldStyle = 1
// const commentedOut = 1
`, []string{"usestate", "onmount", "fileutils", "queryclient", "queryoptions", "appshell",
		"setcount", "datarows", "isloading", "tmpvalue", "routetable", "routesmap", "userprops", "rowkind",
		"datastore", "oldstyle"},
		[]string{"react", "count", "usequery", "helpercall", "othername", "username", "basestore",
			"commentedout", "tanstack/react-query", "./fileutils"})
	if KindOf("dist/app.min.js") != None {
		t.Error("minified file read")
	}
}

func TestGo(t *testing.T) {
	check(t, "internal/x/x.go", `package x

import "github.com/acme/fooBar"

import (
	"context"
	sqlDriver "database/sql/driver"
)

// FileHash is a hash.
type FileHash [32]byte

type (
	chunkInfo struct {
		innerField int
	}
	pathKey string
)

const maxBytes = 1 << 20

const (
	modeAuto, modeHybrid = "auto", "hybrid"
	plainName            = 1
)

var (
	errGone = errors.New("gone")
)

func (s *Store) ReplaceFile(ctx context.Context) error {
	var localThing int
	return nil
}

func newIndexer() {}
`, []string{"foobar", "sqldriver", "filehash", "chunkinfo", "pathkey", "maxbytes", "modeauto", "modehybrid",
		"errgone", "replacefile", "newindexer", "plainname"},
		[]string{"innerfield", "localthing", "context", "store", "x"})
}

func TestVendoredAndLimits(t *testing.T) {
	for _, rel := range []string{"vendor/github.com/x/y.go", "web/node_modules/react/index.js", "a/third_party/b.py",
		"README.md", "notes.txt", "Makefile"} {
		if IsSource(rel) {
			t.Errorf("IsSource(%q)", rel)
		}
	}
	long := "a" + strings.Repeat("B", MaxNameBytes)
	check(t, "a.go", "package a\nfunc "+long+"() {}\nfunc okName() {}\n", []string{"okname"}, []string{strings.ToLower(long)})
	// A huge line (minified code) is skipped, not parsed.
	check(t, "b.js", "const "+strings.Repeat("x", maxLine)+"Yy = 1\nconst fineName = 2\n", []string{"finename"}, nil)
}

// exact checks that src, read as rel, gives exactly want.
func exact(t *testing.T, rel, src string, want ...string) {
	t.Helper()
	got := Extract(rel, []byte(src))
	if want == nil {
		want = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("%s: got %q, want %q", rel, got, want)
	}
}

// TestNoValues: only names in name positions are kept, never a literal, a
// statement after an import, or text inside comments and strings.
func TestNoValues(t *testing.T) {
	// A statement after ; on an import line.
	exact(t, "a.py", "import os; API_TOKEN = 'ghp_SecretTokenValue9'\n")
	exact(t, "a2.py", "import alphaMod, betaMod as aliasName  # also gammaMod\nimport x = 'zetaSecretVal'\n",
		"aliasname", "alphamod", "betamod")
	// An unclosed list ends at the first line that is not a list line.
	exact(t, "b.py", "from x import (\n    yThing,\nPASSWORD = 'hunter2IsMyPassword'\nclass LaterClass: pass\n",
		"laterclass", "ything")
	exact(t, "b2.py", "from x import (\n    firstName,\n    otherSecretWord junk\n    thirdName,\n)\n",
		"firstname")
	exact(t, "b3.py", "from pkgThing import aName, bName as cName, \\\n    dName\nfrom m import (oneName, twoName) junkValue\n",
		"aname", "bname", "cname", "pkgthing")
	// Docstrings and other triple-quoted strings.
	exact(t, "c.py", `def realFunc():
    """Example:

    import creds as dbPassHunter2
    class FakeClass: pass
    """
    return 1
x = '''one line '''
class AfterDoc: pass
y = '''
from z import leakedName
'''
`, "afterdoc", "realfunc")
	// A list cut at maxListLines.
	var b strings.Builder
	b.WriteString("from m import (\n")
	for i := range maxListLines + 5 {
		fmt.Fprintf(&b, "    itemName%03d,\n", i)
	}
	if got := Extract("long.py", []byte(b.String())); len(got) != maxListLines {
		t.Errorf("long list: %d names", len(got))
	}

	exact(t, "a.js", "import x from 'y'; const tok = 'abcDefGhi'\nconst aKey = 'abcDefGhi', bKey = otherVal\n", "akey")
	exact(t, "b.ts", "import {\n  alphaThing,\nexport const holder = { tokenValue: 'qwSecretKey' }\nsecretWordX\n",
		"alphathing")
	exact(t, "b2.ts", "import {\n  alphaThing,\n  zSecretWord: 'x',\n  betaThing,\n} from 'm'\n", "alphathing")
	exact(t, "c.js", `/* import { secretValueX } from 'y'
const hiddenName = 1
*/
const realName = 1 /* const inlineName = 2 */
const tpl = `+"`"+`
export const tplName = 1
`+"`"+`
const url = "http://example.com/const fakeName"
const { someKey: boundName, otherKey = defaultVal } = obj
import 'sideEffectMod'
export default 'stringDefault'
export { aaName as bbName } from 'zz'
`, "aaname", "bbname", "boundname", "otherkey", "realname")

	exact(t, "a.go", "package a\n\nvar apiKey = \"zzzTopSecret\"\nconst (\n\tlongSecret = `\nfunc hiddenFunc() {}\nvar rawName = 1\n`\n\tnextName = 2\n)\n/*\nfunc commentFunc() {}\n*/\nfunc realFunc() {}\n",
		"apikey", "longsecret", "nextname", "realfunc")
	exact(t, "b.go", "package b\n\nimport (\n\t\"github.com/acme/fooBar\"\n\tsecretName = \"zzzTopSecret\"\n\t\"github.com/acme/afterEnd\"\n)\n",
		"foobar")
}
