package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// TerminalPrompter reads answers from in and writes prompts to out. On a
// terminal, secret answers are read with echo turned off. When in is a pipe
// (a script), answers are read line by line, so `goflow login` still works.
type TerminalPrompter struct {
	in  *os.File
	r   *bufio.Reader
	out io.Writer
}

// NewTerminalPrompter returns a prompter for in, usually os.Stdin, and out,
// usually os.Stderr so prompts never mix with a command's real output.
func NewTerminalPrompter(in *os.File, out io.Writer) *TerminalPrompter {
	return &TerminalPrompter{in: in, r: bufio.NewReader(in), out: out}
}

func (p *TerminalPrompter) Prompt(label string, secret bool) (string, error) {
	fmt.Fprint(p.out, label)

	if secret && term.IsTerminal(int(p.in.Fd())) {
		b, err := term.ReadPassword(int(p.in.Fd()))
		fmt.Fprintln(p.out) // the Enter key wasn't echoed either
		return string(b), err
	}

	line, err := p.r.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
