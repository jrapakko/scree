package vocab

import (
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
`, []string{"psycopg", "flask", "requests", "pydantic", "torch", "uvicorn"},
		[]string{"binary", "pool", "base.txt", "editable", "https", "local", "2.3.0"})
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
	if n := len(stop); n < 40 || n > 80 {
		t.Errorf("stoplist has %d words", n)
	}
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
