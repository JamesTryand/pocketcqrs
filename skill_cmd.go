package main

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

// skillFS carries the agent skills into the binary.
//
// This exists because of how PocketCQRS is actually used. The skill lives at
// .claude/skills/, which Claude Code discovers when someone has this repo
// checked out — good for contributors, and useless for the larger audience
// that runs `go install` and never clones anything. Those users have the
// binary and nothing else, so the binary is the one channel that reaches
// them.
//
// `all:` is belt and braces, not a requirement — checked, because the
// obvious guess is wrong in both directions. go:embed omits dot-prefixed
// entries it finds while WALKING a tree, but the pattern here names
// `.claude/skills` explicitly and nothing inside it is dot-prefixed, so a
// plain pattern embeds the same files today. `all:` keeps that true if a
// dotfile is ever added inside a skill.
//
//go:embed all:.claude/skills
var skillFS embed.FS

const skillRoot = ".claude/skills"

// newSkillCommand builds the `skill` CLI command group.
func newSkillCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skill",
		Short: "Install the agent skills that ship with this binary",
		Long: "Agent skills for Claude Code, shipped inside the binary.\n\n" +
			"They are also in the repo at .claude/skills/, which Claude Code picks up\n" +
			"automatically if you cloned it. Install them when you did not — when you\n" +
			"ran `go install` and write your functions in a directory of your own.",
	}

	cmd.AddCommand(newSkillListCommand())
	cmd.AddCommand(newSkillInstallCommand())
	return cmd
}

func newSkillListCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the skills this binary carries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			names, err := skillNames()
			if err != nil {
				return err
			}
			for _, n := range names {
				cmd.Println(n)
			}
			return nil
		},
	}
}

func newSkillInstallCommand() *cobra.Command {
	var dir string
	var force bool

	c := &cobra.Command{
		Use:   "install [name...]",
		Short: "Write the skills to your Claude Code skills directory",
		Long: "Writes each skill to <dir>/<name>/. With no names, installs all of them.\n\n" +
			"The default target is your user-level skills directory, so the skill is\n" +
			"available in every project. Pass --dir .claude/skills to install into one\n" +
			"project instead.\n\n" +
			"Existing files are left alone unless --force, so a skill you have edited\n" +
			"is never overwritten silently.",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := dir
			if target == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return fmt.Errorf("cannot locate your home directory (use --dir): %w", err)
				}
				target = filepath.Join(home, ".claude", "skills")
			}

			available, err := skillNames()
			if err != nil {
				return err
			}
			wanted := args
			if len(wanted) == 0 {
				wanted = available
			}
			for _, name := range wanted {
				if !slices.Contains(available, name) {
					return fmt.Errorf("no skill named %q (this binary carries: %s)",
						name, strings.Join(available, ", "))
				}
			}

			written, skipped := 0, 0
			for _, name := range wanted {
				w, s, err := installSkill(cmd, name, target, force)
				if err != nil {
					return err
				}
				written += w
				skipped += s
			}

			switch {
			case written == 0 && skipped > 0:
				cmd.Printf("\nNothing written: %d file(s) already exist. Re-run with --force to overwrite.\n", skipped)
			case skipped > 0:
				cmd.Printf("\nInstalled to %s (%d file(s) written, %d left alone — --force overwrites).\n",
					target, written, skipped)
			default:
				cmd.Printf("\nInstalled to %s (%d file(s)).\n", target, written)
				if dir == "" {
					cmd.Println("Available in every project. Claude Code picks it up on its next start.")
				} else {
					cmd.Println("Available in projects rooted at that directory, on Claude Code's next start.")
				}
			}
			return nil
		},
	}

	c.Flags().StringVar(&dir, "dir", "",
		"install into this directory instead of your user-level skills directory")
	c.Flags().BoolVar(&force, "force", false, "overwrite files that already exist")
	return c
}

// installSkill copies one skill's tree out of the binary, returning how many
// files were written and how many were left alone.
func installSkill(cmd *cobra.Command, name, target string, force bool) (int, int, error) {
	src := skillRoot + "/" + name
	written, skipped := 0, 0

	err := fs.WalkDir(skillFS, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(filepath.FromSlash(src), filepath.FromSlash(p))
		if err != nil {
			return err
		}
		dst := filepath.Join(target, name, rel)

		if _, err := os.Stat(dst); err == nil && !force {
			cmd.Printf("  exists, left alone  %s\n", dst)
			skipped++
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		data, err := skillFS.ReadFile(p)
		if err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
		cmd.Printf("  wrote               %s\n", dst)
		written++
		return nil
	})
	return written, skipped, err
}

// skillNames lists the skill directories carried in the binary. A skill is a
// directory holding a SKILL.md; the README that sits alongside them is
// documentation for a reader of the repo, not something to install.
func skillNames() ([]string, error) {
	entries, err := fs.ReadDir(skillFS, skillRoot)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := fs.Stat(skillFS, skillRoot+"/"+e.Name()+"/SKILL.md"); err != nil {
			continue
		}
		out = append(out, e.Name())
	}
	return out, nil
}
