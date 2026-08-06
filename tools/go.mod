// The tools module exists to keep the root module's go.mod empty.
//
// Rendering the generated tables needs tablewriter, and Go has no way to mark a
// requirement as tool-only: any module that requires it hands the checksums to
// every consumer's go.sum, whether or not a line of it is ever compiled. A module
// boundary is the only thing that stops that, so everything with a dependency
// lives here and the library ships requiring nothing at all.
module github.com/W-Floyd/go-mp3packer/tools

go 1.26.5

require github.com/W-Floyd/go-mp3packer v0.0.0

require github.com/olekukonko/tablewriter v1.1.4

require (
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/clipperhouse/displaywidth v0.10.0 // indirect
	github.com/clipperhouse/uax29/v2 v2.6.0 // indirect
	github.com/fatih/color v1.18.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/mattn/go-runewidth v0.0.19 // indirect
	github.com/olekukonko/cat v0.0.0-20250911104152-50322a0618f6 // indirect
	github.com/olekukonko/errors v1.2.0 // indirect
	github.com/olekukonko/ll v0.1.6 // indirect
	golang.org/x/sys v0.30.0 // indirect
)

replace github.com/W-Floyd/go-mp3packer => ../
