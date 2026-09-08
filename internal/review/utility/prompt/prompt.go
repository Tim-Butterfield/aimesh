// Package prompt provides Prompter implementations (the input Utility the wizard
// uses). Both implementations satisfy the documented review.Prompter contract.
package prompt

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/Tim-Butterfield/aimesh/internal/review"
)

// Stdin is an interactive Prompter backed by an io.Reader/Writer.
type Stdin struct {
	In  *bufio.Reader
	Out io.Writer
}

// NewStdin builds an interactive Prompter.
func NewStdin(in io.Reader, out io.Writer) *Stdin {
	return &Stdin{In: bufio.NewReader(in), Out: out}
}

func (s *Stdin) line(prompt string) string {
	fmt.Fprint(s.Out, prompt+" ")
	text, _ := s.In.ReadString('\n')
	return strings.TrimSpace(text)
}

func (s *Stdin) Ask(prompt string) string { return s.line(prompt) }

func (s *Stdin) Choose(prompt string, options []string) int {
	for i, o := range options {
		fmt.Fprintf(s.Out, "  [%d] %s\n", i+1, o)
	}
	n, err := strconv.Atoi(s.line(prompt))
	if err != nil || n < 1 || n > len(options) {
		return 0
	}
	return n - 1
}

func (s *Stdin) Confirm(prompt string) bool {
	a := strings.ToLower(s.line(prompt + " [y/N]"))
	return a == "y" || a == "yes"
}

func (s *Stdin) AskPath(prompt string) string { return s.line(prompt) }

// Scripted is a deterministic Prompter for tests: it returns queued answers.
type Scripted struct {
	Answers    []string // for Ask/AskPath
	Choices    []int    // for Choose
	Confirms   []bool   // for Confirm
	ai, ci, fi int
}

func (s *Scripted) next(xs []string, i *int) string {
	if *i < len(xs) {
		v := xs[*i]
		*i++
		return v
	}
	return ""
}

func (s *Scripted) Ask(string) string     { return s.next(s.Answers, &s.ai) }
func (s *Scripted) AskPath(string) string { return s.next(s.Answers, &s.ai) }

func (s *Scripted) Choose(string, []string) int {
	if s.ci < len(s.Choices) {
		v := s.Choices[s.ci]
		s.ci++
		return v
	}
	return 0
}

func (s *Scripted) Confirm(string) bool {
	if s.fi < len(s.Confirms) {
		v := s.Confirms[s.fi]
		s.fi++
		return v
	}
	return false
}

// Compile-time confirmation that both satisfy the documented contract.
var (
	_ review.Prompter = (*Stdin)(nil)
	_ review.Prompter = (*Scripted)(nil)
)
