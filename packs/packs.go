// Package packs implements domain packs: portable bundles of a domain's
// function files (pb_functions) plus any plain (non-projection-owned)
// collection schemas, with a manifest. Projection-owned collections are
// never packed — they are recreated from //@schema directives on import
// (boot reconcile / maintenance reload).
//
// Trust model: packs are trusted, owner-authored code (goja in-process) —
// only import packs you trust. Importing UNTRUSTED packs is the documented
// trigger for the isolated-runtime (wasm) decision gate.
package packs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"

	"pocketcqrs/functions"
)

// Manifest describes a domain pack.
type Manifest struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	GeneratedAt string   `json:"generatedAt"`
	// Functions lists the pb_functions basenames in the pack.
	Functions []string `json:"functions"`
	// Collections lists plain (non-projection-owned) collections whose
	// schemas ship in collections.json (PocketBase's native import format).
	Collections []string `json:"collections,omitempty"`
}

const (
	manifestFile    = "manifest.json"
	collectionsFile = "collections.json"
	functionsDirName = "pb_functions"
)

// ExportOptions controls Export.
type ExportOptions struct {
	Name        string   // pack name (required)
	Version     string   // default "0.1.0"
	Description string
	Functions   []string // pb_functions basenames; empty = all .js files
	Collections []string // plain collections to include
	// GuardedCollections are projection-owned collections — they are
	// refused (they are recreated from //@schema on import).
	GuardedCollections []string
}

// Export writes a pack directory at outDir: manifest.json, pb_functions/
// with the function sources, and collections.json if collections were named.
func Export(app core.App, functionsDir, outDir string, opts ExportOptions) (*Manifest, error) {
	if opts.Name == "" || strings.ContainsAny(opts.Name, `/\`) {
		return nil, fmt.Errorf("packs: invalid pack name %q", opts.Name)
	}
	if opts.Version == "" {
		opts.Version = "0.1.0"
	}

	// ---- function files ----
	var fnames []string
	if len(opts.Functions) > 0 {
		fnames = opts.Functions
	} else {
		entries, err := os.ReadDir(functionsDir)
		if err != nil {
			return nil, fmt.Errorf("packs: read functions dir: %w", err)
		}
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".js") {
				fnames = append(fnames, e.Name())
			}
		}
	}
	if len(fnames) == 0 {
		return nil, fmt.Errorf("packs: no function files to pack (dir %s)", functionsDir)
	}
	sort.Strings(fnames)

	sources := map[string][]byte{}
	for _, name := range fnames {
		raw, err := os.ReadFile(filepath.Join(functionsDir, name))
		if err != nil {
			return nil, fmt.Errorf("packs: %s: %w", name, err)
		}
		sources[name] = raw
	}

	// validate the files load cleanly: a pack that doesn't parse is not
	// worth shipping (boot/reload would refuse it anyway)
	checkDir, err := os.MkdirTemp("", "packcheck")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(checkDir)
	for name, raw := range sources {
		if err := os.WriteFile(filepath.Join(checkDir, name), raw, 0o644); err != nil {
			return nil, err
		}
	}
	if _, err := functions.LoadDir(functions.NewGojaRuntime(nil), app, checkDir); err != nil {
		return nil, fmt.Errorf("packs: pack files do not load: %w", err)
	}

	// ---- collections ----
	guarded := map[string]bool{}
	for _, name := range opts.GuardedCollections {
		guarded[name] = true
	}
	var marshaled []json.RawMessage
	for _, name := range opts.Collections {
		if guarded[name] || strings.HasPrefix(name, "_") {
			return nil, fmt.Errorf("packs: collection %q is projection-owned or a system collection; "+
				"projection collections are recreated from //@schema on import", name)
		}
		col, err := app.FindCollectionByNameOrId(name)
		if err != nil {
			return nil, fmt.Errorf("packs: collection %q not found: %w", name, err)
		}
		raw, err := json.Marshal(col)
		if err != nil {
			return nil, err
		}
		marshaled = append(marshaled, raw)
	}

	// ---- write the pack ----
	manifest := &Manifest{
		Name:        opts.Name,
		Version:     opts.Version,
		Description: opts.Description,
		GeneratedAt: time.Now().UTC().Format("2006-01-02 15:04:05.000Z"),
		Functions:   fnames,
		Collections: opts.Collections,
	}
	if err := os.MkdirAll(filepath.Join(outDir, functionsDirName), 0o755); err != nil {
		return nil, err
	}
	mraw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, manifestFile), append(mraw, '\n'), 0o644); err != nil {
		return nil, err
	}
	for name, raw := range sources {
		if err := os.WriteFile(filepath.Join(outDir, functionsDirName, name), raw, 0o644); err != nil {
			return nil, err
		}
	}
	if len(marshaled) > 0 {
		craw, err := json.Marshal(marshaled)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(outDir, collectionsFile), craw, 0o644); err != nil {
			return nil, err
		}
	}
	return manifest, nil
}

// ImportResult summarizes an Import.
type ImportResult struct {
	Manifest            Manifest
	FunctionsCopied     []string
	FunctionsSkipped    []string
	CollectionsImported int
}

// Import installs a pack: function files are copied into functionsDir
// (existing files are skipped unless force) and collections.json is applied
// via PocketBase's native collection import (never deleting missing ones).
// Function files are load-validated BEFORE anything is copied. The event
// store is untouched; decider validation and schema reconcile happen on the
// next boot or maintenance reload.
func Import(app core.App, functionsDir, packDir string, force bool) (*ImportResult, error) {
	mraw, err := os.ReadFile(filepath.Join(packDir, manifestFile))
	if err != nil {
		return nil, fmt.Errorf("packs: %s: %w", manifestFile, err)
	}
	var manifest Manifest
	if err := json.Unmarshal(mraw, &manifest); err != nil {
		return nil, fmt.Errorf("packs: invalid manifest: %w", err)
	}
	if manifest.Name == "" || len(manifest.Functions) == 0 {
		return nil, fmt.Errorf("packs: manifest needs a name and at least one function file")
	}

	// load-validate the pack's functions before touching the target dir
	packFns := filepath.Join(packDir, functionsDirName)
	if _, err := functions.LoadDir(functions.NewGojaRuntime(nil), app, packFns); err != nil {
		return nil, fmt.Errorf("packs: pack files do not load: %w", err)
	}

	result := &ImportResult{Manifest: manifest}
	if err := os.MkdirAll(functionsDir, 0o755); err != nil {
		return nil, err
	}
	for _, name := range manifest.Functions {
		src := filepath.Join(packFns, name)
		dst := filepath.Join(functionsDir, name)
		raw, err := os.ReadFile(src)
		if err != nil {
			return result, fmt.Errorf("packs: %s listed in manifest but missing: %w", name, err)
		}
		if !force {
			if _, err := os.Stat(dst); err == nil {
				result.FunctionsSkipped = append(result.FunctionsSkipped, name)
				continue
			}
		}
		if err := os.WriteFile(dst, raw, 0o644); err != nil {
			return result, err
		}
		result.FunctionsCopied = append(result.FunctionsCopied, name)
	}

	craw, err := os.ReadFile(filepath.Join(packDir, collectionsFile))
	if err == nil && len(craw) > 0 {
		var arr []json.RawMessage
		if err := json.Unmarshal(craw, &arr); err != nil {
			return result, fmt.Errorf("packs: invalid %s: %w", collectionsFile, err)
		}
		if len(arr) > 0 {
			if err := app.ImportCollectionsByMarshaledJSON(craw, false); err != nil {
				return result, fmt.Errorf("packs: collections import failed: %w", err)
			}
			result.CollectionsImported = len(arr)
		}
	} else if err != nil && !os.IsNotExist(err) {
		return result, err
	}

	return result, nil
}
