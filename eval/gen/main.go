// Command gen writes the seeded synthetic corpus Generate (gen.go) builds
// to a pair of JSONL files: an events file and a labels file, the
// event-replay pair shape eval.LoadReplayDataset reads (task brief).
//
//	go run ./eval/gen -seed 20260927 \
//	  -out-events eval/fixtures/synthetic/events.jsonl \
//	  -out-labels eval/fixtures/synthetic/labels.jsonl
//
// The committed eval/fixtures/synthetic/*.jsonl were produced by exactly
// this command at the seed above; re-running it with the same seed
// reproduces them byte-for-byte (gen_test.go's TestGenerate_Deterministic
// checks this at the Go-value level, not by shelling out to this binary).
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "gen:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	seed := fs.Int64("seed", 20260927, "deterministic RNG seed")
	outEvents := fs.String("out-events", "", "path to write the generated events JSONL (required)")
	outLabels := fs.String("out-labels", "", "path to write the generated labels JSONL (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *outEvents == "" || *outLabels == "" {
		return fmt.Errorf("usage: gen -seed N -out-events path -out-labels path")
	}

	res := Generate(Options{Seed: *seed})

	if err := writeJSONL(*outEvents, len(res.Events), func(i int) any { return res.Events[i] }); err != nil {
		return fmt.Errorf("write events: %w", err)
	}
	if err := writeJSONL(*outLabels, len(res.Labels), func(i int) any { return res.Labels[i] }); err != nil {
		return fmt.Errorf("write labels: %w", err)
	}

	families := map[string]int{}
	for _, f := range res.FamilyOf {
		families[f]++
	}
	names := make([]string, 0, len(families))
	for n := range families {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("gen: wrote %d events, %d labelled subjects across %d families (seed=%d)\n", len(res.Events), len(res.Labels), len(names), *seed)
	for _, n := range names {
		fmt.Printf("  %-40s %d\n", n, families[n])
	}
	return nil
}

// writeJSONL writes n rows (get(i) returns the i-th) to path, one
// json.Marshal'd object per line.
func writeJSONL(path string, n int, get func(i int) any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for i := 0; i < n; i++ {
		b, err := json.Marshal(get(i))
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	return w.Flush()
}
