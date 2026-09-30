package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tokencanopy/abusekit/eval"
	"github.com/tokencanopy/abusekit/internal/config"
	"github.com/tokencanopy/abusekit/internal/feature"
)

func runGolden(f evalFlags, cfg *config.Config, brands feature.BrandSet, webmail feature.WebmailSet) error {
	info, err := os.Stat(f.dataset)
	if err != nil {
		return exitCode2(err)
	}
	if err := validateGoldenOutput(f, info.IsDir()); err != nil {
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
		if err := writeGoldenFile(f.out, out.Bytes()); err != nil {
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

// resolvedPath also resolves parent-directory symlinks for a new output file.
func resolvedPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err == nil {
		return resolved, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return "", err
	}
	parent, err = resolvedPath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, filepath.Base(abs)), nil
}

func validateGoldenOutput(f evalFlags, directory bool) error {
	if f.out == "" {
		return nil
	}
	output, err := resolvedPath(f.out)
	if err != nil {
		return err
	}
	inputs := []string{f.dataset, f.goldenCheck, f.rulesPath, f.vendorsPath, f.weightsPath, f.brandsPath, f.brandsExtraPath, f.webmailPath}
	if directory {
		dir, err := resolvedPath(f.dataset)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, output)
		if err != nil {
			return err
		}
		if rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return fmt.Errorf("golden output must be outside the fixture directory")
		}
		paths, err := filepath.Glob(filepath.Join(f.dataset, "*.jsonl"))
		if err != nil {
			return err
		}
		inputs = append(inputs, paths...)
		inputs = append(inputs, filepath.Join(f.dataset, "synthetic", "events.jsonl"))
	}
	outputInfo, err := os.Stat(f.out)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, input := range inputs {
		if input == "" {
			continue
		}
		resolved, err := resolvedPath(input)
		if err != nil {
			return err
		}
		inputInfo, err := os.Stat(input)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if output == resolved || (inputInfo != nil && outputInfo != nil && os.SameFile(inputInfo, outputInfo)) {
			return fmt.Errorf("golden output must differ from every input and reference")
		}
	}
	return nil
}

func writeGoldenFile(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".golden-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(0644); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
