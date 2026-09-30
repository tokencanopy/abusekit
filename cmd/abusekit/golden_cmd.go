package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
)

func runGolden(f evalFlags, cfg *config.Config, brands feature.BrandSet, webmail feature.WebmailSet) error {
	info, err := os.Stat(f.dataset)
	if err != nil {
		return exitCode2(err)
	}
	var out bytes.Buffer
	if info.IsDir() {
		err = eval.WriteGoldenFixtures(context.Background(), &out, f.dataset, cfg, brands, webmail)
	} else {
		input, openErr := os.Open(f.dataset)
		if openErr != nil {
			return exitCode2(openErr)
		}
		err = eval.WriteGolden(context.Background(), &out, filepath.Base(f.dataset), input, cfg, brands, webmail)
		input.Close()
	}
	if err != nil {
		return exitCode2(err)
	}
	if f.goldenCheck != "" {
		want, err := os.ReadFile(f.goldenCheck)
		if err != nil {
			return exitCode2(err)
		}
		if !bytes.Equal(out.Bytes(), want) {
			return exitCode1(fmt.Errorf("golden drift from %s: first differing line %d", f.goldenCheck, firstDifferentLine(out.Bytes(), want)))
		}
	}
	if f.out != "" {
		// A check must never overwrite its own reference, even on success.
		outputPath, _ := filepath.Abs(f.out)
		inputPath, _ := filepath.Abs(f.dataset)
		referencePath, _ := filepath.Abs(f.goldenCheck)
		if outputPath == inputPath || (f.goldenCheck != "" && outputPath == referencePath) {
			return exitCode2(fmt.Errorf("golden output must differ from input and reference"))
		}
		if err := os.MkdirAll(filepath.Dir(f.out), 0755); err != nil {
			return exitCode2(err)
		}
		if err := os.WriteFile(f.out, out.Bytes(), 0644); err != nil {
			return exitCode2(err)
		}
	} else if f.goldenCheck == "" {
		if _, err := os.Stdout.Write(out.Bytes()); err != nil {
			return exitCode2(err)
		}
	}
	return nil
}

func firstDifferentLine(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	line := 1
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return line
		}
		if a[i] == '\n' {
			line++
		}
	}
	return line
}
