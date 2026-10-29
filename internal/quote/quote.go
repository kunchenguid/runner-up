// Package quote quotes words for POSIX and Windows command lines.
package quote

import (
	"fmt"
	"strings"
)

// ShQuote quotes s for a POSIX shell.
func ShQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// WinPlain are the characters a word may hold to reach a Windows program
// unchanged from both cmd and PowerShell without quotes.
const WinPlain = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-.:\\/=+"

// WinQuote writes s so that cmd and PowerShell both hand it to the program as
// one argument: plain words as they are, others in double quotes. Neither
// shell has a quote that keeps every character, so checkWinWord refuses the
// characters that would still change inside double quotes; a setup holding
// one never loads.
func WinQuote(s string) string {
	if s != "" && strings.Trim(s, WinPlain) == "" {
		return s
	}
	return `"` + s + `"`
}

// CheckWinWord refuses a word winQuote cannot pass on unchanged: inside double
// quotes cmd still expands %, PowerShell $ and `, and " ends the quote. The
// program's own name must be plain: PowerShell does not run a quoted word.
func CheckWinWord(s string, program bool) error {
	if program && (s == "" || strings.Trim(s, WinPlain) != "") {
		return fmt.Errorf("%q must be a plain program name or path, with no spaces, on a Windows machine", s)
	}
	if strings.ContainsAny(s, "\"%$`\n\r") || strings.HasSuffix(s, `\`) {
		return fmt.Errorf("%q cannot pass to a Windows machine's shell unchanged", s)
	}
	return nil
}
