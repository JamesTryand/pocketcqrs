package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The embed must actually carry the skill. A wrong pattern is a compile
// error, but a pattern that matches the tree while silently skipping the
// dot-prefixed path is not — which is the specific trap here, since every
// path lives under .claude and go:embed omits dot-prefixed entries when
// walking a tree unless the pattern says `all:`.
func TestBinaryCarriesTheSkill(t *testing.T) {
	names, err := skillNames()
	if err != nil {
		t.Fatalf("skillNames: %v", err)
	}
	if len(names) == 0 {
		t.Fatal("the binary carries no skills; the go:embed pattern matched nothing useful")
	}
	if !contains(names, "pocketcqrs-domain") {
		t.Fatalf("pocketcqrs-domain is not embedded, got %v", names)
	}

	// and it is the real file, not an empty placeholder
	body, err := skillFS.ReadFile(skillRoot + "/pocketcqrs-domain/SKILL.md")
	if err != nil {
		t.Fatalf("reading the embedded skill: %v", err)
	}
	for _, want := range []string{"name: pocketcqrs-domain", "description:", "## The invariant"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("the embedded SKILL.md is missing %q", want)
		}
	}
}

// The copy in the binary must match the copy in the repo. go:embed reads at
// build time so they cannot diverge in a built binary — but this catches the
// case where someone adds a second copy somewhere and edits the wrong one.
func TestEmbeddedSkillMatchesTheRepo(t *testing.T) {
	onDisk, err := os.ReadFile(filepath.Join(".claude", "skills", "pocketcqrs-domain", "SKILL.md"))
	if err != nil {
		t.Fatalf("reading the repo copy: %v", err)
	}
	embedded, err := skillFS.ReadFile(skillRoot + "/pocketcqrs-domain/SKILL.md")
	if err != nil {
		t.Fatalf("reading the embedded copy: %v", err)
	}
	if !bytes.Equal(onDisk, embedded) {
		t.Error("the embedded skill differs from the one in .claude/skills/")
	}
}

func TestSkillInstallWritesAndRefusesToClobber(t *testing.T) {
	dir := t.TempDir()

	run := func(args ...string) string {
		t.Helper()
		cmd := newSkillCommand()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("skill %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}

	// first install writes the file
	out := run("install", "--dir", dir)
	target := filepath.Join(dir, "pocketcqrs-domain", "SKILL.md")
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("nothing installed at %s: %v\n%s", target, err, out)
	}
	if !strings.Contains(out, "wrote") {
		t.Errorf("install did not report what it wrote:\n%s", out)
	}

	// a second install leaves an edited file alone rather than clobbering it
	if err := os.WriteFile(target, []byte("MINE"), 0o644); err != nil {
		t.Fatal(err)
	}
	out = run("install", "--dir", dir)
	body, _ := os.ReadFile(target)
	if string(body) != "MINE" {
		t.Error("a second install overwrote an edited skill without --force")
	}
	if !strings.Contains(out, "--force") {
		t.Errorf("the skip did not say how to override it:\n%s", out)
	}

	// --force overwrites
	run("install", "--dir", dir, "--force")
	body, _ = os.ReadFile(target)
	if string(body) == "MINE" {
		t.Error("--force did not overwrite")
	}
}

func TestSkillInstallRejectsAnUnknownName(t *testing.T) {
	cmd := newSkillCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"install", "--dir", t.TempDir(), "no-such-skill"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("an unknown skill name was accepted")
	}
	if !strings.Contains(err.Error(), "pocketcqrs-domain") {
		t.Errorf("the error does not name what IS available: %v", err)
	}
}

func TestSkillList(t *testing.T) {
	cmd := newSkillCommand()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "pocketcqrs-domain") {
		t.Errorf("list did not name the skill: %q", out.String())
	}
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
