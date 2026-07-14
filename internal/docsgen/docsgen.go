// Package docsgen renders HASteward's reference documentation from authoritative
// in-code sources — the Cobra command tree and the env binding registry — as markdown
// fragments assembled into the docs site by the pipeline's narrate step.
//
// It is a small framework: each reference (CLI, environment, and — as HASteward grows —
// CRDs, metrics, labels, exit codes, config) is a Generator registered here. The
// `hasteward docs generate` command stays stable and just runs every registered
// generator, so adding a reference is a new file, not a new command.
package docsgen

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// Generator renders one reference fragment (e.g. cli-reference.md).
type Generator struct {
	Name  string // fragment basename without extension, e.g. "cli-reference"
	Title string // human label, for generation logs
	Fn    func(root *cobra.Command, w io.Writer) error
}

var generators []Generator

// Register adds a generator to the set GenerateAll runs. Called from each generator's init().
func Register(g Generator) { generators = append(generators, g) }

// Generators returns the registered generators in registration order.
func Generators() []Generator { return generators }

// GenerateAll runs every registered generator, writing <outputDir>/<name>.md, and
// returns the paths written. A failing generator aborts with its partial output removed.
func GenerateAll(root *cobra.Command, outputDir string) ([]string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return nil, fmt.Errorf("creating output dir %q: %w", outputDir, err)
	}
	written := make([]string, 0, len(generators))
	for _, g := range generators {
		path := filepath.Join(outputDir, g.Name+".md")
		f, err := os.Create(path)
		if err != nil {
			return written, fmt.Errorf("creating %s: %w", path, err)
		}
		if err := g.Fn(root, f); err != nil {
			f.Close()
			os.Remove(path)
			return written, fmt.Errorf("generating %s: %w", g.Name, err)
		}
		if err := f.Close(); err != nil {
			return written, fmt.Errorf("closing %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
}
