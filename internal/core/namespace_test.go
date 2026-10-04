package core_test

import (
	"encoding/json"
	"github.com/tokencanopy/abusekit/internal/core"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
)

func TestVectorCustomHashQuantumPreservesScorerValues(t *testing.T) {
	r := rule("example", "advise", withInputs("custom.elapsed"))
	a := core.Plan(core.Vector{Values: map[string]float64{"custom.elapsed": 25}, HashQuantum: map[string]float64{"custom.elapsed": 10}}, nil, []core.RuleState{{Rule: r}})
	b := core.Plan(core.Vector{Values: map[string]float64{"custom.elapsed": 29}, HashQuantum: map[string]float64{"custom.elapsed": 10}}, nil, []core.RuleState{{Rule: r, LastInputHash: a[0].InputHash}})
	if !b[0].Skip || b[0].Request.Features["custom.elapsed"] != 29 {
		t.Fatal("metadata quantum must bucket hash only")
	}
	c := core.Plan(core.Vector{Values: map[string]float64{"custom.elapsed": 30}, HashQuantum: map[string]float64{"custom.elapsed": 10}}, nil, []core.RuleState{{Rule: r, LastInputHash: a[0].InputHash}})
	if c[0].Skip {
		t.Fatal("quantum boundary did not rescore")
	}
}

func TestCoreHasNoFlatFeatureLiterals(t *testing.T) {
	b, err := os.ReadFile("../../eval/testdata/rename-map.json")
	if err != nil {
		t.Fatal(err)
	}
	var old map[string]string
	if err = json.Unmarshal(b, &old); err != nil {
		t.Fatal(err)
	}
	files, err := parser.ParseDir(token.NewFileSet(), ".", func(info os.FileInfo) bool { return !strings.HasSuffix(info.Name(), "_test.go") }, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range files {
		for name, f := range pkg.Files {
			ast.Inspect(f, func(n ast.Node) bool {
				if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
					v, _ := strconv.Unquote(lit.Value)
					if next, exists := old[v]; exists {
						t.Errorf("%s uses flat %q; want %q", name, v, next)
					}
				}
				return true
			})
		}
	}
}
