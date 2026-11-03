// Package cli parses one command's flags the same way for every runner-up
// command: -h shows the command's own flags and exits 0, a bad flag or an
// argument the command does not take exits 2, and both are said on standard
// error.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// Parse parses args into fs, a flag set made with flag.ContinueOnError. When
// it returns stop, the command ends with code: 0 after -h, 2 after a bad flag,
// or 2 after arguments left over when positional is false, as for a command that
// takes flags only.
func Parse(fs *flag.FlagSet, args []string, positional bool) (code int, stop bool) {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	if !positional && fs.NArg() > 0 {
		fmt.Fprintf(fs.Output(), "%s takes no arguments, only flags; got %s\n", fs.Name(), strings.Join(fs.Args(), " "))
		return 2, true
	}
	return 0, false
}

// Help reports whether args asks for help, as -h, -help or --help.
func Help(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "-help" || args[0] == "--help" || args[0] == "help")
}
