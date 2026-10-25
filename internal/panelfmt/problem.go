package panelfmt

import (
	"strconv"
	"unicode/utf8"
)

// Format renders the problem as `<file>:<line>[:<col>]: <msg>`, the shape the
// footer note and --check print (for example `panel.conf:3:14: unknown token
// {foo}`). Col 0 means the problem belongs to the whole line and prints no
// column; Line 0 means it belongs to the whole file (too big, unreadable) and
// prints neither.
func (p Problem) Format(file string) string {
	switch {
	case p.Line <= 0:
		return file + ": " + p.Msg
	case p.Col <= 0:
		return file + ":" + strconv.Itoa(p.Line) + ": " + p.Msg
	default:
		return file + ":" + strconv.Itoa(p.Line) + ":" + strconv.Itoa(p.Col) + ": " + p.Msg
	}
}

// problems collects findings in file order.
type problems []Problem

func (ps *problems) fatal(line, col int, msg string) {
	*ps = append(*ps, Problem{Line: line, Col: col, Msg: msg, Fatal: true})
}

func (ps *problems) soft(line, col int, msg string) {
	*ps = append(*ps, Problem{Line: line, Col: col, Msg: msg})
}

func (ps problems) hasFatal() bool {
	for _, p := range ps {
		if p.Fatal {
			return true
		}
	}
	return false
}

// runeCol is the 1-based column of byte offset off in line, counted in runes
// (what an editor's status bar shows), not bytes and not display cells.
func runeCol(line string, off int) int {
	if off > len(line) {
		off = len(line)
	}
	return utf8.RuneCountInString(line[:off]) + 1
}
