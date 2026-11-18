// Command docgen renders the slagctl command tree as a markdown reference.
// Run via `just gen-cli-docs`; CI fails if the committed copy is stale, so
// the reference cannot drift from the code.
package main

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bluesky-social/slagworks/cmd/slagctl/cmd"
)

func main() {
	out := os.Stdout
	if len(os.Args) > 1 {
		// Build-time dev tool: the output path is the operator's own argv,
		// not untrusted input.
		f, err := os.Create(os.Args[1]) //nolint:gosec // G703: argv-supplied output path is intended
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}

	root := cmd.NewRootCmd()
	root.InitDefaultHelpCmd()
	root.InitDefaultCompletionCmd()

	var b strings.Builder
	b.WriteString("# slagctl command reference\n\n")
	b.WriteString("Status: generated — do not edit. Regenerate with `just gen-cli-docs`.\n\n")
	b.WriteString(root.Short + "\n\n")
	writeGlobalFlags(&b, root)
	walk(&b, root)
	fmt.Fprint(out, b.String())
}

func writeGlobalFlags(b *strings.Builder, root *cobra.Command) {
	fl := root.PersistentFlags()
	if !fl.HasAvailableFlags() {
		return
	}
	b.WriteString("## Global flags\n\n```text\n")
	b.WriteString(strings.TrimRight(fl.FlagUsages(), "\n"))
	b.WriteString("\n```\n\n")
}

func walk(b *strings.Builder, c *cobra.Command) {
	for _, sub := range sortedChildren(c) {
		writeCommand(b, sub)
		walk(b, sub)
	}
}

func sortedChildren(c *cobra.Command) []*cobra.Command {
	var out []*cobra.Command
	for _, sub := range c.Commands() {
		if !sub.IsAvailableCommand() || sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		out = append(out, sub)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

func writeCommand(b *strings.Builder, c *cobra.Command) {
	depth := strings.Count(c.CommandPath(), " ")
	level := min(depth+1, 4)
	fmt.Fprintf(b, "%s %s\n\n", strings.Repeat("#", level+1), c.CommandPath())
	if c.Short != "" {
		b.WriteString(c.Short + "\n\n")
	}
	if c.Long != "" && c.Long != c.Short {
		b.WriteString(strings.TrimSpace(c.Long) + "\n\n")
	}
	fmt.Fprintf(b, "```text\n%s\n```\n\n", c.UseLine())
	if fl := c.NonInheritedFlags(); fl.HasAvailableFlags() {
		b.WriteString("Flags:\n\n```text\n")
		b.WriteString(strings.TrimRight(fl.FlagUsages(), "\n"))
		b.WriteString("\n```\n\n")
	}
}
