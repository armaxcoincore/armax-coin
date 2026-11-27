package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
)

// maxInputFileSize bounds every JSON input (policy, ledger, claims, manifest) so a hostile file
// cannot exhaust memory. A full Testnet manifest is well below this.
const maxInputFileSize = 64 << 20

// readStrictJSON decodes exactly one JSON value from path into v, rejecting unknown fields and
// trailing data so typos in governance inputs fail loudly instead of being ignored.
func readStrictJSON(path string, v any) error {
	// Errors name only the base file name: claim errors end up in a published manifest, which
	// must not reveal local directory layouts.
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", filepath.Base(path), stripPath(err))
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxInputFileSize+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", filepath.Base(path), stripPath(err))
	}
	if len(data) > maxInputFileSize {
		return fmt.Errorf("%s exceeds %d bytes", filepath.Base(path), maxInputFileSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse %s: trailing data after JSON value", filepath.Base(path))
	}
	return nil
}

// stripPath drops the path from a *fs.PathError and keeps the underlying cause.
func stripPath(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}

// writeJSONFile writes v as indented JSON through a temporary file and rename, so a crash never
// leaves a truncated manifest that could be mistaken for a complete one.
func writeJSONFile(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".armreconcile-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// addArmoids adds two non-negative amounts and fails instead of wrapping.
func addArmoids(a, b int64) (int64, error) {
	if a < 0 || b < 0 {
		return 0, errors.New("negative amount")
	}
	if a > math.MaxInt64-b {
		return 0, errors.New("amount overflow")
	}
	return a + b, nil
}
