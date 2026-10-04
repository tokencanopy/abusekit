package eval

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// P0 remains immutable. P1 may change only feature keys, hashes, and versions.
func TestGoldenRenameParity(t *testing.T) {
	raw, err := os.ReadFile("testdata/rename-map.json")
	if err != nil {
		t.Fatal(err)
	}
	var names map[string]string
	if err = json.Unmarshal(raw, &names); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-arm64", "-amd64-no-fma"} {
		t.Run(suffix, func(t *testing.T) {
			read := func(prefix string) []GoldenRow {
				b, err := os.ReadFile(filepath.Join("golden", prefix+suffix+".jsonl"))
				if err != nil {
					t.Fatal(err)
				}
				s := bufio.NewScanner(bytes.NewReader(b))
				var rows []GoldenRow
				for s.Scan() {
					var row GoldenRow
					if err = json.Unmarshal(s.Bytes(), &row); err != nil {
						t.Fatal(err)
					}
					rows = append(rows, row)
				}
				if err = s.Err(); err != nil {
					t.Fatal(err)
				}
				return rows
			}
			old, next := read("reference-flat"), read("reference-ns")
			if len(old) != 8671 || len(next) != len(old) {
				t.Fatalf("replay length changed: %d / %d", len(old), len(next))
			}
			for i, a := range old {
				b := next[i]
				renamed := map[string]string{}
				for k, v := range a.Features {
					n, ok := names[k]
					if !ok {
						t.Fatalf("unmapped feature %s", k)
					}
					renamed[n] = v
				}
				a.Features = renamed
				if len(a.Rules) != len(b.Rules) {
					t.Fatalf("row %d rule count changed", i)
				}
				for j := range a.Rules {
					if a.Rules[j].Version == b.Rules[j].Version || a.Rules[j].InputHash == b.Rules[j].InputHash {
						t.Fatalf("row %d rename did not update identity", i)
					}
					a.Rules[j].Version = b.Rules[j].Version
					a.Rules[j].InputHash = b.Rules[j].InputHash
				}
				if !reflect.DeepEqual(a, b) {
					t.Fatalf("row %d changed beyond names and identities", i)
				}
			}
		})
	}
}
