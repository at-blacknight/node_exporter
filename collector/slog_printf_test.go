// Copyright The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package collector

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// slogMsgArg lists the log/slog logging methods and the position of their
// message argument.
var slogMsgArg = map[string]int{
	"Debug": 0, "Info": 0, "Warn": 0, "Error": 0,
	"DebugContext": 1, "InfoContext": 1, "WarnContext": 1, "ErrorContext": 1,
	"Log": 2, "LogAttrs": 2,
}

// printfVerb matches a Printf directive. %% is escaped and not a directive, so
// it is stripped before matching.
//
// The space flag is deliberately excluded: "% d" is a valid directive, but
// accepting a space here would flag ordinary prose such as "disk is 90% full"
// as "% f". Percentages are common in log messages and a directive written with
// a space flag is not, so the trade is worth it. See TestPrintfVerbPattern.
var printfVerb = regexp.MustCompile(`%[+\-#0-9.]*[bcdeEfFgGopqstTvwxXU]`)

// notALogger are receiver names whose same-named methods are not slog's, most
// importantly testing.T and testing.B.
var notALogger = map[string]bool{"t": true, "b": true, "fmt": true}

// TestNoPrintfVerbsInSlogMessages guards against passing a Printf format string
// to slog, which does no formatting: the verbs are logged verbatim and the
// arguments meant to fill them are consumed as key/value pairs instead, so
// values land in key position and the record can carry duplicate or
// iteration-dependent keys.
//
// go vet's slog analyzer already rejects most of this mistake: it fires when a
// key-position argument is not a string, or when the arguments do not pair up.
// What it cannot see is the case where the count happens to be even and every
// key-position argument happens to be a string, because then the call is
// well-formed by its rules. That residual case is what this test covers, and it
// is the one that shipped in v1.12.1 logging "%s/%s/%s: %w" literally.
//
// Only literal messages are checked; a format string held in a variable is not
// detected. Tightening that would need type or dataflow analysis.
func TestNoPrintfVerbsInSlogMessages(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	for _, file := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}

		// Parsed as source rather than loaded as a package so that files for
		// every GOOS are checked, not just the ones this platform builds.
		parsed, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}

		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			msg, pos, ok := slogMessageArg(call)
			if !ok {
				return true
			}
			if verb := printfVerb.FindString(strings.ReplaceAll(msg, "%%", "")); verb != "" {
				t.Errorf("%s: slog message contains the Printf verb %s: %q\n"+
					"\tslog does not format messages; pass the values as key/value pairs instead",
					fset.Position(pos), verb, msg)
			}
			return true
		})
	}
}

// TestPrintfVerbPattern pins down what the matcher does and does not treat as a
// directive, including the prose cases it must not flag.
func TestPrintfVerbPattern(t *testing.T) {
	for _, tc := range []struct {
		msg   string
		match bool
	}{
		{"failed to read %s", true},
		{"got %d items", true},
		{"value %+v", true},
		{"padded %-10s", true},
		{"wrapped: %w", true},
		{"couldn't get ue_count for controller/csrow/channel %s/%s/%s: %w", true},

		{"disk is 90% full", false},
		{"cache hit rate 50% degraded", false},
		{"shrunk by 10% overnight", false},
		{"progress at 100%%", false},
		{"couldn't get ue_count", false},
		{"100% of requests", false},
	} {
		got := printfVerb.MatchString(strings.ReplaceAll(tc.msg, "%%", ""))
		if got != tc.match {
			t.Errorf("printfVerb.MatchString(%q) = %v, want %v", tc.msg, got, tc.match)
		}
	}
}

// slogMessageArg returns the literal message of a slog logging call.
func slogMessageArg(call *ast.CallExpr) (msg string, pos token.Pos, ok bool) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", 0, false
	}
	idx, ok := slogMsgArg[sel.Sel.Name]
	if !ok || len(call.Args) <= idx {
		return "", 0, false
	}
	if notALogger[receiverName(sel.X)] {
		return "", 0, false
	}
	lit, ok := call.Args[idx].(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", 0, false
	}
	unquoted, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", 0, false
	}
	return unquoted, lit.Pos(), true
}

// receiverName reduces the expression a method is called on to a single name:
// "logger" for c.logger.Debug, "slog" for slog.Info, "t" for t.Error.
func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	}
	return ""
}
