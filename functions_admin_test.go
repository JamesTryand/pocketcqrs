package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JamesTryand/pocketcqrs/functions"
)

// TestResolveFunctionPathRejects is the security test for the function-file
// API: everything behind resolveFunctionPath writes code the server will
// execute, so the name check is the whole control surface. Each of these
// must be refused — and refused by the resolver, not by luck downstream.
func TestResolveFunctionPathRejects(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{
		"",
		".js",              // no stem
		".hidden.js",       // leading dot
		"../escape.js",     // traversal, posix
		"..\\escape.js",    // traversal, windows
		"../../etc/pwn.js", // deeper traversal
		"sub/nested.js",    // subdirectory
		"sub\\nested.js",
		"/abs.js",
		"C:\\windows\\system32\\evil.js",
		"..",
		"...",
		"plain.txt",  // not a function file
		"noext",      //
		"pb_hooks/x", //
		"CON.js",     // windows device names resolve as devices, not files
		"nul.js",
		"com1.js",
		"LPT1.js",
		"sp ace.js",  // spaces are not in the allowed set
		"quo\"te.js", //
		"semi;colon.js",
		"new\nline.js",
	} {
		if got, err := resolveFunctionPath(dir, name); err == nil {
			t.Errorf("resolveFunctionPath(%q) was accepted and resolved to %q; it must be refused", name, got)
		}
	}
}

// TestResolveFunctionPathAccepts: ordinary names still work, and always land
// directly inside the functions directory.
func TestResolveFunctionPathAccepts(t *testing.T) {
	dir := t.TempDir()

	for _, name := range []string{
		"audit.js", "task_audit.js", "orders-by-customer.js",
		"a.js", "note.v2.js", "Order9.js",
	} {
		got, err := resolveFunctionPath(dir, name)
		if err != nil {
			t.Errorf("resolveFunctionPath(%q) was refused: %v", name, err)
			continue
		}
		if filepath.Dir(got) != filepath.Clean(dir) {
			t.Errorf("resolveFunctionPath(%q) escaped the functions dir: %q", name, got)
		}
		if filepath.Base(got) != name {
			t.Errorf("resolveFunctionPath(%q) renamed the file to %q", name, filepath.Base(got))
		}
	}
}

// TestListFunctionFilesClassifies: the listing has to agree with the loader
// about what each file is, and must SHOW a file that fails to parse rather
// than hide it — that file blocks every reload, so it is exactly the one an
// operator is looking for.
func TestListFunctionFilesClassifies(t *testing.T) {
	dir := t.TempDir()
	write := func(name, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("audit.js", "//@trigger event TaskCreated\nconsole.log('x')\n")
	write("hello.js", "//@trigger http\nfunction handler() {}\n")
	write("beat.js", "//@trigger cron * * * * *\nconsole.log('tick')\n")
	write("rollup.js", "//@trigger projection rollup on TaskCreated\n//@schema rollups total:number\n//@key total\nfunction project() {}\n")
	write("note.js", "//@trigger decider note\n//@handles NoteCreated\nfunction decide() {}\n")
	write("broken.js", "//@trigger\nconsole.log('no kind')\n")
	write("plain.js", "console.log('no directives at all')\n")
	write("notes.txt", "not a function file")

	files, err := listFunctionFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]functionFile{}
	for _, f := range files {
		byName[f.Name] = f
	}
	if _, ok := byName["notes.txt"]; ok {
		t.Error("non-.js files should not be listed")
	}
	if len(files) != 7 {
		t.Fatalf("expected 7 .js files listed, got %d", len(files))
	}

	for name, want := range map[string]functions.Kind{
		"audit.js":  functions.KindEffect,
		"hello.js":  functions.KindEffect,
		"beat.js":   functions.KindEffect,
		"rollup.js": functions.KindProjection,
		"note.js":   functions.KindDecider,
		"plain.js":  functions.KindNone,
	} {
		f := byName[name]
		if f.Declaration == nil {
			t.Errorf("%s: no declaration (error: %s)", name, f.Error)
			continue
		}
		if f.Declaration.Kind != want {
			t.Errorf("%s: classified as %q, want %q", name, f.Declaration.Kind, want)
		}
	}

	// the schema-bearing flag is what tells the UI whether activation needs
	// the maintenance barrier
	for name, want := range map[string]bool{
		"audit.js": false, "hello.js": false, "beat.js": false,
		"rollup.js": true, "note.js": true,
	} {
		if got := byName[name].Declaration.SchemaBearing; got != want {
			t.Errorf("%s: schemaBearing=%v, want %v", name, got, want)
		}
	}

	if byName["rollup.js"].Declaration.Projection != "rollup" ||
		len(byName["rollup.js"].Declaration.Collections) != 1 {
		t.Errorf("projection declaration incomplete: %+v", byName["rollup.js"].Declaration)
	}
	if byName["note.js"].Declaration.Aggregate != "note" {
		t.Errorf("decider aggregate not reported: %+v", byName["note.js"].Declaration)
	}

	// a file that does not parse is listed WITH its error
	if b := byName["broken.js"]; b.Declaration != nil || b.Error == "" {
		t.Errorf("broken.js should be listed with an error, got %+v", b)
	}

	// every listed file carries its size and timestamp
	if byName["audit.js"].Size == 0 || byName["audit.js"].Modified == "" {
		t.Errorf("file metadata missing: %+v", byName["audit.js"])
	}
}

// TestCheckFunctionSourceRefusesBadCron is the regression test for a file
// that could be WRITTEN and then abort every later reload — defeating the
// whole point of refusing unloadable writes. Compiling is not the whole
// check for the effect tier: the cron service parses the schedule too, and
// it does so after the effect tier has already been swapped, so a bad one
// used to leave a reload half-applied as well.
func TestCheckFunctionSourceRefusesBadCron(t *testing.T) {
	c := &components{} // the effect tier needs no app and no store

	for _, src := range []string{
		"//@trigger cron 99 99 99 99 99\nconsole.log('tick');\n",
		"//@trigger cron not a schedule at all\nconsole.log('tick');\n",
		"//@trigger cron * * *\nconsole.log('tick');\n", // too few segments
	} {
		if _, err := c.checkFunctionSource("badcron.js", src); err == nil {
			t.Errorf("an unusable cron schedule was accepted: %q", strings.SplitN(src, "\n", 2)[0])
		}
	}

	// a real schedule still passes, macros included
	for _, src := range []string{
		"//@trigger cron * * * * *\nconsole.log('tick');\n",
		"//@trigger cron 0 3 * * 1\nconsole.log('weekly');\n",
		"//@trigger cron @daily\nconsole.log('macro');\n",
	} {
		if _, err := c.checkFunctionSource("goodcron.js", src); err != nil {
			t.Errorf("a valid schedule was refused: %q: %v", strings.SplitN(src, "\n", 2)[0], err)
		}
	}
}

// TestDeclaresReportsBadSchema: the listing exists to surface the file that
// blocks reloads, so a malformed //@schema must not render as a healthy
// projection that merely owns no collections.
func TestDeclaresReportsBadSchema(t *testing.T) {
	dir := t.TempDir()
	src := "//@trigger projection p on X\n//@schema\n//@key a\nfunction project() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "badschema.js"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := functions.Declares("badschema.js", src); err == nil {
		t.Error("a malformed //@schema was reported as a clean declaration")
	}
	files, err := listFunctionFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Error == "" || files[0].Declaration != nil {
		t.Errorf("the listing should show the file with its error, got %+v", files[0])
	}
}

// TestDeclaresMatchesLoaderTolerance pins the one quirk deliberately left in
// place: cron is not counted by the single-purpose check, so a cron+projection
// file is still tolerated (projection wins) rather than becoming a boot
// failure. If this ever changes it should be a decision, not a side effect.
func TestDeclaresMatchesLoaderTolerance(t *testing.T) {
	d, err := functions.Declares("odd.js",
		"//@trigger cron * * * * *\n//@trigger projection p on X\n//@schema ps a:number\n//@key a\n")
	if err != nil {
		t.Fatalf("cron+projection should still be tolerated: %v", err)
	}
	if d.Kind != functions.KindProjection {
		t.Errorf("projection should win the classification, got %q", d.Kind)
	}

	// projection + decider remains refused
	if _, err := functions.Declares("bad.js",
		"//@trigger projection p on X\n//@trigger decider a\n"); err == nil ||
		!strings.Contains(err.Error(), "single-purpose") {
		t.Errorf("projection+decider must be refused as not single-purpose, got %v", err)
	}
}
