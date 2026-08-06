// Package mdtable renders the markdown tables that the repository's documents
// carry, and swaps them into place between the HTML comments that mark where
// each one lives.
//
// It exists so that the one dependency any of this needs — tablewriter — is
// reachable from the tools module alone. The library and the command it ships
// import nothing outside the standard library, and keeping the renderer here is
// what holds that true all the way out to a consumer's go.sum: a module that
// requires nothing puts nothing in one.
//
// Both generators used to carry their own copy of this. They rendered the same
// way by coincidence rather than by construction, which is not a property to
// leave to chance in files whose whole purpose is that their columns line up.
package mdtable

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/renderer"
	"github.com/olekukonko/tablewriter/tw"
)

// Align is a column's alignment. It and the two values below are re-exported so
// that a caller building a table needs no import of tablewriter at all.
type Align = tw.Align

var (
	Left  = tw.AlignLeft
	Right = tw.AlignRight
)

// Render lays a table out as markdown. Columns are aligned by align, which is
// also what markdown's own alignment row will say, so a column of numbers lines
// up on its digits wherever the file is rendered.
//
// Header formatting is switched off: the default upper-cases them, which reads as
// shouting in prose and loses the case of things like `-n`.
func Render(header []string, align []Align, rows [][]string) (string, error) {
	var buf bytes.Buffer
	cell := tw.CellConfig{Alignment: tw.CellAlignment{PerColumn: align}}
	t := tablewriter.NewTable(&buf,
		tablewriter.WithRenderer(renderer.NewMarkdown()),
		tablewriter.WithConfig(tablewriter.Config{
			Header: tw.CellConfig{
				Formatting: tw.CellFormatting{AutoFormat: tw.Off},
				Alignment:  cell.Alignment,
			},
			Row: cell,
		}))
	t.Header(header)
	for _, r := range rows {
		if err := t.Append(r); err != nil {
			return "", err
		}
	}
	if err := t.Render(); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Replace swaps the text between the markers for id, which are HTML comments of
// the form `<!-- prefix:id -->` and `<!-- /prefix:id -->` so that they do not
// render. Their presence is required rather than created: silently appending a
// table to the end of a document is never what was wanted.
func Replace(text, prefix, id, body string) (string, error) {
	open := "<!-- " + prefix + ":" + id + " -->"
	shut := "<!-- /" + prefix + ":" + id + " -->"
	i, j := strings.Index(text, open), strings.Index(text, shut)
	if i < 0 || j < 0 || j < i {
		return "", fmt.Errorf("markers for %q not found in the document", id)
	}
	return text[:i+len(open)] + "\n" + body + text[j:], nil
}
