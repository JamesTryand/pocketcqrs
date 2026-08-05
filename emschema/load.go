package emschema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Manifest describes how one document is split across files. Order lives
// here and ONLY here — it is deliberately not repeated as a per-slice index
// field, which would be two sources of truth able to disagree.
type Manifest struct {
	EventModelingSchemaVersion string            `json:"eventModelingSchemaVersion"`
	ID                         string            `json:"id"`
	Name                       string            `json:"name"`
	Description                string            `json:"description,omitempty"`
	Swimlanes                  []Swimlane        `json:"swimlanes"`
	Registries                 map[string]string `json:"registries,omitempty"`
	Slices                     []string          `json:"slices,omitempty"`
}

// Load reads a document from path, which may be either a single JSON
// document or a directory containing a manifest.json.
//
// Import accepts both; export emits a single document. Splitting on export
// is the schema project's own tooling's job and buys this project nothing.
func Load(path string) (*Document, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return LoadManifest(path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("emschema: %s is not a valid document: %w", filepath.Base(path), err)
	}
	return &doc, nil
}

// LoadManifest joins a manifest directory into the single-document shape.
//
// This is a Go reimplementation of the schema project's scripts/join.js,
// which is ~40 lines of mechanical work — small enough that reimplementing
// it is cheaper than depending on node at import time. Registry and slice
// files are referenced by EXPLICIT PATH, never discovered by naming
// convention, which is what lets someone lay slice files out however suits
// them without any tooling change.
func LoadManifest(dir string) (*Document, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, fmt.Errorf("emschema: %s does not look like a split document (no manifest.json): %w", dir, err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("emschema: invalid manifest.json: %w", err)
	}

	doc := &Document{
		EventModelingSchemaVersion: m.EventModelingSchemaVersion,
		ID:                         m.ID,
		Name:                       m.Name,
		Description:                m.Description,
		Swimlanes:                  m.Swimlanes,
	}

	// registry paths are relative to the manifest's own directory, and a
	// path escaping it is refused: a manifest is data, and data must not be
	// able to name a file outside the tree it came from
	load := func(rel string, into any) error {
		full, err := resolveWithin(dir, rel)
		if err != nil {
			return err
		}
		body, err := os.ReadFile(full)
		if err != nil {
			return fmt.Errorf("emschema: manifest references %s: %w", rel, err)
		}
		if err := json.Unmarshal(body, into); err != nil {
			return fmt.Errorf("emschema: %s: %w", rel, err)
		}
		return nil
	}

	for key, rel := range m.Registries {
		var err error
		switch key {
		case "events":
			err = load(rel, &doc.Events)
		case "commands":
			err = load(rel, &doc.Commands)
		case "readModels":
			err = load(rel, &doc.ReadModels)
		case "screens":
			err = load(rel, &doc.Screens)
		case "automations":
			err = load(rel, &doc.Automations)
		case "chapters":
			err = load(rel, &doc.Chapters)
		case "actorLanes":
			err = load(rel, &doc.ActorLanes)
		case "hotspots":
			err = load(rel, &doc.Hotspots)
		default:
			// refused rather than ignored: a registry key this version does
			// not know is a document that means more than we can import,
			// and importing the rest silently would lose it without saying so
			err = fmt.Errorf("emschema: manifest declares an unknown registry %q", key)
		}
		if err != nil {
			return nil, err
		}
	}

	for _, rel := range m.Slices {
		var s Slice
		if err := load(rel, &s); err != nil {
			return nil, err
		}
		doc.Slices = append(doc.Slices, s)
	}
	return doc, nil
}

// resolveWithin joins rel onto dir and refuses anything that escapes it.
func resolveWithin(dir, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("emschema: manifest has an empty file path")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("emschema: manifest path %q must be relative to the manifest", rel)
	}
	base := filepath.Clean(dir)
	full := filepath.Clean(filepath.Join(base, rel))
	relCheck, err := filepath.Rel(base, full)
	if err != nil || relCheck == ".." || len(relCheck) > 2 && relCheck[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("emschema: manifest path %q resolves outside the document directory", rel)
	}
	return full, nil
}
