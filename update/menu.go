package update

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/pkg/term"
	"golang.org/x/sys/unix"

	"github.com/weaveworks/flux"
)

// Escape sequences.
const (
	moveCursorUp    = "\033[%dA"
	moveStartOfLine = "\r"
	hideCursor      = "\033[?25l"
	showCursor      = "\033[?25h"

	tableHeading = "CONTROLLER \tSTATUS \tUPDATES"
)

type WriteFlusher interface {
	io.Writer
	Flush() error
}

type ClearableLineWriter struct {
	wf    WriteFlusher
	lines int    // line count
	width uint16 // terminal width
}

func NewClearableWriter(wf WriteFlusher) *ClearableLineWriter {
	w := &ClearableLineWriter{wf: wf, lines: 0, width: 9999}
	ws, _ := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if ws != nil && ws.Col != 0 {
		w.width = ws.Col
	}
	return w
}

// Writeln counts the lines we output.
func (c *ClearableLineWriter) Writeln(line string) error {
	line += "\n"
	c.lines += (len(line)-1)/int(c.width) + 1
	_, err := c.wf.Write([]byte(line))
	return err
}

// Clear moves the terminal cursor up to the beginning of the
// line where we started writing.
func (c *ClearableLineWriter) Clear() {
	if c.lines == 0 {
		return
	}
	fmt.Fprintf(c.wf, moveCursorUp, c.lines)
	fmt.Fprintf(c.wf, moveStartOfLine)
	c.lines = 0
}

func (c *ClearableLineWriter) Flush() error {
	return c.wf.Flush()
}

type menuItem struct {
	id     flux.ResourceID
	status ControllerUpdateStatus
	error  string
	update ContainerUpdate

	selected bool
}

// Menu presents a list of controllers which can be interacted with.
type Menu struct {
	out        *ClearableLineWriter
	items      []menuItem
	selectable int
	cursor     int
}

// NewMenu creates a menu printer that outputs a result set to
// the `io.Writer` provided, at the given level of verbosity:
//  - 2 = include skipped and ignored resources
//  - 1 = include skipped resources, exclude ignored resources
//  - 0 = exclude skipped and ignored resources
//
// It can print a one time listing with `Print()` or then enter
// interactive mode with `Run()`.
func NewMenu(out io.Writer, results Result, verbosity int) *Menu {
	m := &Menu{
		out: NewClearableWriter(tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)),
	}
	m.fromResults(results, verbosity)
	return m
}

func (m *Menu) fromResults(results Result, verbosity int) {
	for _, serviceID := range results.ServiceIDs() {
		resourceID := flux.MustParseResourceID(serviceID)
		result := results[resourceID]
		switch result.Status {
		case ReleaseStatusIgnored:
			if verbosity < 2 {
				continue
			}
		case ReleaseStatusSkipped:
			if verbosity < 1 {
				continue
			}
		}

		if result.Error != "" {
			m.AddItem(menuItem{
				id:     resourceID,
				status: result.Status,
				error:  result.Error,
			})
		}
		for _, upd := range result.PerContainer {
			m.AddItem(menuItem{
				id:     resourceID,
				status: result.Status,
				update: upd,
			})
		}
		if result.Error == "" && len(result.PerContainer) == 0 {
			m.AddItem(menuItem{
				id:     resourceID,
				status: result.Status,
			})
		}
	}
	return
}

func (m *Menu) AddItem(mi menuItem) {
	if mi.selectable() {
		mi.selected = true
		m.selectable++
	}
	m.items = append(m.items, mi)
}

// Run starts the interactive menu mode.
func (m *Menu) Run() (map[flux.ResourceID][]ContainerUpdate, error) {
	specs := make(map[flux.ResourceID][]ContainerUpdate)
	if m.selectable == 0 {
		return specs, errors.New("No changes found.")
	}

	m.printInteractive()
	fmt.Printf(hideCursor)
	defer fmt.Printf(showCursor)

	for {
		ascii, keyCode, err := getChar()
		if err != nil {
			return specs, err
		}

		switch ascii {
		case 3, 27, 'q':
			return specs, errors.New("Aborted.")
		case ' ':
			m.toggleCursor()
		case 13:
			for _, item := range m.items {
				if item.selected {
					specs[item.id] = append(specs[item.id], item.update)
				}
			}
			m.out.Writeln("")
			return specs, nil
		case 9, 'j':
			m.cursorDown()
		case 'k':
			m.cursorUp()
		}

		switch keyCode {
		case 40:
			m.cursorDown()
		case 38:
			m.cursorUp()
		}
	}
}

func (m *Menu) Print() {
	m.out.Writeln(tableHeading)
	var previd flux.ResourceID
	for _, item := range m.items {
		inline := previd == item.id
		m.out.Writeln(m.renderItem(item, inline))
		previd = item.id
	}
	m.out.Flush()
}

func (m *Menu) printInteractive() {
	m.out.Clear()
	m.out.Writeln("   " + tableHeading)
	i := 0
	var previd flux.ResourceID
	for _, item := range m.items {
		inline := previd == item.id
		m.out.Writeln(m.renderInteractiveItem(item, inline, i))
		previd = item.id
		if item.selectable() {
			i++
		}
	}
	m.out.Writeln("")
	m.out.Writeln("Use [Space] to deselect containers and hit [Enter] to release selected.")

	m.out.Flush()
}

func (m *Menu) renderItem(item menuItem, inline bool) string {
	if inline {
		return fmt.Sprintf("\t\t%s", item.updates())
	} else {
		return fmt.Sprintf("%s\t%s\t%s", item.id, item.status, item.updates())
	}
}

func (m *Menu) renderInteractiveItem(item menuItem, inline bool, index int) string {
	pre := bytes.Buffer{}
	if index == m.cursor {
		pre.WriteString(">")
	} else {
		pre.WriteString(" ")
	}
	pre.WriteString(item.checkbox())
	pre.WriteString(" ")
	pre.WriteString(m.renderItem(item, inline))

	return pre.String()
}

func (m *Menu) toggleCursor() {
	m.items[m.cursor].selected = !m.items[m.cursor].selected
	m.printInteractive()
}

func (m *Menu) cursorDown() {
	m.cursor = (m.cursor + 1) % m.selectable
	m.printInteractive()
}

func (m *Menu) cursorUp() {
	m.cursor = (m.cursor + m.selectable - 1) % m.selectable
	m.printInteractive()
}

func (i menuItem) checkbox() string {
	switch {
	case !i.selectable():
		return " "
	case i.selected:
		return "\u25c9"
	default:
		return "\u25ef"
	}
}

func (i menuItem) selectable() bool {
	return i.update.Container != ""
}

func (i menuItem) updates() string {
	if i.update.Container != "" {
		return fmt.Sprintf("%s: %s -> %s",
			i.update.Container,
			i.update.Current.String(),
			i.update.Target.Tag)
	}
	return i.error
}

// See https://github.com/paulrademacher/climenu/blob/master/getchar.go
func getChar() (ascii int, keyCode int, err error) {
	t, _ := term.Open("/dev/tty")
	term.RawMode(t)
	bs := make([]byte, 3)

	var numRead int
	numRead, err = t.Read(bs)
	if err != nil {
		return
	}
	if numRead == 3 && bs[0] == 27 && bs[1] == 91 {
		// Three-character control sequence, beginning with "ESC-[".

		// Since there are no ASCII codes for arrow keys, we use
		// Javascript key codes.
		if bs[2] == 65 {
			// Up
			keyCode = 38
		} else if bs[2] == 66 {
			// Down
			keyCode = 40
		} else if bs[2] == 67 {
			// Right
			keyCode = 39
		} else if bs[2] == 68 {
			// Left
			keyCode = 37
		}
	} else if numRead == 1 {
		ascii = int(bs[0])
	} else {
		// Two characters read??
	}
	t.Restore()
	t.Close()
	return
}
