package appstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"selfupdate/internal/fsutil"
)

type Schema struct {
	Current int

	Migrate map[int]func(data json.RawMessage) (json.RawMessage, error)
}

var ErrCorrupt = errors.New("saved state is corrupt")

var ErrMigration = errors.New("state migration failed")

type NewerSchemaError struct{ Found, Supported int }

func (e *NewerSchemaError) Error() string {
	return fmt.Sprintf("saved state has schema %d but this build only understands up to %d "+
		"(was the program rolled back?)", e.Found, e.Supported)
}

const schemaKey = "_schema"

func SaveVersioned(path string, s Schema, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return writeEnvelope(path, s.Current, data)
}

func writeEnvelope(path string, version int, data json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return errors.New("versioned state must encode as a JSON object")
	}
	if _, clash := obj[schemaKey]; clash {
		return fmt.Errorf("state may not use the reserved key %q", schemaKey)
	}
	obj[schemaKey] = json.RawMessage(strconv.Itoa(version))
	b, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(path, b, 0o600))
}

func corrupt(path string, cause error) error {
	kept, rerr := fsutil.Quarantine(path)
	if rerr != nil {
		return fmt.Errorf("%w (and could not be preserved: %v): %v", ErrCorrupt, rerr, cause)
	}
	return fmt.Errorf("%w (moved to %s): %v", ErrCorrupt, kept, cause)
}

func LoadVersioned(path string, s Schema, v any) error {
	raw, err := fsutil.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	found, data, err := parseEnvelope(raw)
	if err != nil {
		return corrupt(path, err)
	}
	if found > s.Current {
		return &NewerSchemaError{Found: found, Supported: s.Current}
	}
	if found < 0 {
		return corrupt(path, fmt.Errorf("negative schema %d", found))
	}
	migrated := found < s.Current
	for n := found; n < s.Current; n++ {
		step, ok := s.Migrate[n]
		if !ok {
			return fmt.Errorf("%w: no migration from schema %d", ErrMigration, n)
		}
		if data, err = step(data); err != nil {
			return fmt.Errorf("%w: schema %d -> %d: %v", ErrMigration, n, n+1, err)
		}
	}
	if err := json.Unmarshal(data, v); err != nil {
		if migrated {
			return fmt.Errorf("%w: result does not decode: %v", ErrMigration, err)
		}
		return corrupt(path, err)
	}
	if !migrated {
		return nil
	}

	if err := fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(backupPath(path, found), raw, 0o600)); err != nil {
		return fmt.Errorf("%w: cannot back up old state: %v", ErrMigration, err)
	}
	if err := writeEnvelope(path, s.Current, data); err != nil {
		return fmt.Errorf("%w: cannot write upgraded state: %v", ErrMigration, err)
	}
	return nil
}

func RestoreFromBackup(path string, s Schema) (restoredFrom int, err error) {
	raw, err := fsutil.ReadFile(path)
	if err != nil {
		return 0, err
	}
	found, _, err := parseEnvelope(raw)
	if err != nil {
		return 0, err
	}
	if found <= s.Current {
		return 0, fmt.Errorf("state schema %d is not newer than %d; nothing to restore", found, s.Current)
	}
	for v := s.Current; v >= 0; v-- {
		b, rerr := os.ReadFile(backupPath(path, v))
		if rerr != nil {
			continue
		}
		if _, _, perr := parseEnvelope(b); perr != nil {
			continue
		}
		newer := path + ".v" + strconv.Itoa(found) + ".newer"
		if err := fsutil.Rename(path, newer); err != nil {
			return 0, err
		}
		if err := fsutil.IgnoreDirSync(fsutil.WriteFileAtomic(path, b, 0o600)); err != nil {
			_ = fsutil.Rename(newer, path)
			return 0, err
		}
		return v, nil
	}
	return 0, fmt.Errorf("no usable backup of schema <= %d next to %s", s.Current, path)
}

func backupPath(path string, version int) string {
	return path + ".v" + strconv.Itoa(version) + ".bak"
}

func parseEnvelope(raw []byte) (int, json.RawMessage, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&top); err != nil {
		return 0, nil, err
	}

	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return 0, nil, errors.New("trailing data after state document")
	}
	if top == nil {
		return 0, nil, errors.New("state is not a JSON object")
	}
	if sc, ok := top[schemaKey]; ok {
		n, err := schemaNumber(sc)
		if err != nil {
			return 0, nil, err
		}
		delete(top, schemaKey)
		d, err := json.Marshal(top)
		return n, d, err
	}

	if sc, ok := top["schema"]; ok && len(top) <= 2 {
		d, hasData := top["data"]
		switch {
		case hasData:
			n, err := schemaNumber(sc)
			return n, d, err
		case len(top) == 1:
			return 0, nil, errors.New("versioned state has no data field")
		}
	}
	return 0, bytes.TrimSpace(raw), nil
}

func schemaNumber(raw json.RawMessage) (int, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("bad schema field: %w", err)
	}
	return n, nil
}
