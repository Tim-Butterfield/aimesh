package config

// store.go holds the domain-free config-store I/O: byte-atomic writes, YAML/JSON format
// detection + conversion, strict JSON decode plumbing, and the "validate before load / write"
// mechanism. The schema check is INJECTED via ValidateBytes, so this store never learns any
// app's typed schema.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Tim-Butterfield/aimesh/meshcore/fault"
	"gopkg.in/yaml.v3"
)

// ValidateBytes is the injected schema check: given a file path (for format/extension) and its
// candidate bytes, it returns nil if the bytes satisfy the caller's strict schema, else a fault.
// The store accepts this as a parameter so it never learns the typed schema; a caller supplies a
// validator backed by its own typed decode.
type ValidateBytes func(path string, b []byte) error

// IsYAML reports whether path should be parsed/serialized as YAML (vs JSON), by extension.
func IsYAML(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		return true
	}
	return false
}

// YAMLToJSON converts YAML bytes to JSON bytes (YAML is a superset of JSON, so a single set of
// `json:` tags can stay a caller's schema source of truth for typed decoding).
func YAMLToJSON(b []byte) ([]byte, error) {
	var v any
	if err := yaml.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// DecodeStrict strict-decodes JSON bytes into dst, rejecting unknown struct fields. dst is any so
// the store stays schema-agnostic; callers pass a pointer to their own typed config value.
func DecodeStrict(b []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// LoadRawMap reads a config file into a generic key→value map AFTER the injected validator accepts
// it, so a caller can inspect which keys are explicitly present (vs defaults). Returns the
// validator's error if the file does not satisfy the schema (so a caller never reads a malformed
// source).
func LoadRawMap(path string, validate ValidateBytes) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fault.Wrap(fault.Config, fmt.Sprintf("read config %q", path), err)
	}
	if verr := validate(path, b); verr != nil {
		return nil, verr
	}
	root := map[string]any{}
	if uerr := yaml.Unmarshal(b, &root); uerr != nil {
		return nil, fault.Wrap(fault.Config, fmt.Sprintf("parse config %q", path), uerr)
	}
	return root, nil
}

// ApplyPatchToFile is the single config-mutation write path: it validates the existing config at
// path (if present) with the injected validator, applies the patch to the decoded map (creating
// nested maps, preserving unrelated fields, erroring rather than overwriting a malformed shape),
// ensures `schemaVersion`, and writes the result only after the injected validator re-accepts the
// serialized bytes. It creates the file/dir if absent and never writes secrets. An existing file
// that fails validation is refused and left byte-for-byte unchanged.
func ApplyPatchToFile(path string, patch Patch, validate ValidateBytes) error {
	root := map[string]any{}
	if b, err := os.ReadFile(path); err == nil {
		// Refuse to touch a config that doesn't already validate — surface the schema error
		// rather than silently preserving an unknown/bad key.
		if verr := validate(path, b); verr != nil {
			return verr
		}
		if uerr := yaml.Unmarshal(b, &root); uerr != nil {
			return fault.Wrap(fault.Config, fmt.Sprintf("parse existing config %q", path), uerr)
		}
		if root == nil {
			root = map[string]any{}
		}
	}
	if err := patch.Apply(root); err != nil {
		return err
	}
	if _, ok := root["schemaVersion"]; !ok {
		root["schemaVersion"] = 1
	}
	// Serialize in the target file's format so the strict re-validation below (which picks the
	// parser by extension) matches: YAML for .yaml/.yml, JSON for a legacy .json.
	var out []byte
	var err error
	if IsYAML(path) {
		out, err = yaml.Marshal(root)
	} else {
		out, err = json.MarshalIndent(root, "", "  ")
		out = append(out, '\n') // match the JSON writer's trailing newline for a .json target
	}
	if err != nil {
		return fault.Wrap(fault.Internal, "marshal config", err)
	}
	// The written result must itself satisfy the validator.
	if verr := validate(path, out); verr != nil {
		return fault.Wrap(fault.Config, "refusing to write a config that would not reload", verr)
	}
	return WriteFileAtomic(path, out)
}

// WriteFileAtomic writes already-serialized config bytes to path **atomically**: it creates the
// parent dir, writes a temp file in the SAME directory, then renames it over the target (a rename
// within a filesystem is atomic), so a crash/interruption never leaves a truncated or partially-
// written config. The temp file is removed on every failure path (and is a harmless no-op once
// renamed). It does not parse/validate/mutate — it only writes bytes safely.
func WriteFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fault.Wrap(fault.Config, "create config dir", err)
	}
	tmp, err := os.CreateTemp(dir, ".meshcore-config-*.tmp")
	if err != nil {
		return fault.Wrap(fault.Config, "create temp config", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op once renamed; guarantees cleanup on every error/panic path
	if _, werr := tmp.Write(b); werr != nil {
		tmp.Close()
		return fault.Wrap(fault.Config, "write temp config", werr)
	}
	// fsync the data to disk before the rename so a crash can't leave the renamed target pointing
	// at unflushed (empty/partial) contents. chmod on the open descriptor (not the path) avoids a
	// TOCTOU race.
	if serr := tmp.Sync(); serr != nil {
		tmp.Close()
		return fault.Wrap(fault.Config, "sync temp config", serr)
	}
	if cherr := tmp.Chmod(0o644); cherr != nil {
		tmp.Close()
		return fault.Wrap(fault.Config, "chmod temp config", cherr)
	}
	if cerr := tmp.Close(); cerr != nil {
		return fault.Wrap(fault.Config, "close temp config", cerr)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fault.Wrap(fault.Config, "replace config", err)
	}
	return nil
}
