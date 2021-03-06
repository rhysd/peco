package peco

import (
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var screen Screen = Termbox{}

// CtxOptions is the interface that defines that options can be
// passed in from the command line
type CtxOptions interface {
	// EnableNullSep should return if the null separator is
	// enabled (--null)
	EnableNullSep() bool

	// BufferSize should return the buffer size. By default (i.e.
	// when it returns 0), the buffer size is unlimited.
	// (--buffer-size)
	BufferSize() int

	// InitialIndex is the line number to put the cursor on
	// when peco starts
	InitialIndex() int

	// LayoutType returns the name of the layout to use
	LayoutType() string
}

type PageInfo struct {
	index   int
	offset  int
	perPage int
}

type CaretPosition int

func (p CaretPosition) Int() int {
	return int(p)
}

func (p CaretPosition) CaretPos() CaretPosition {
	return p
}

func (p *CaretPosition) SetCaretPos(where int) {
	*p = CaretPosition(where)
}

func (p *CaretPosition) MoveCaretPos(offset int) {
	*p = CaretPosition(p.Int() + offset)
}

type FilterQuery []rune

func (q FilterQuery) Query() FilterQuery {
	return q
}

func (q FilterQuery) String() string {
	return string(q)
}

func (q FilterQuery) QueryLen() int {
	return len(q)
}

func (q *FilterQuery) AppendQuery(r rune) {
	*q = FilterQuery(append([]rune(*q), r))
}

func (q *FilterQuery) InsertQueryAt(ch rune, where int) {
	sq := []rune(*q)
	buf := make([]rune, q.QueryLen()+1)
	copy(buf, sq[:where])
	buf[where] = ch
	copy(buf[where+1:], sq[where:])
	*q = FilterQuery(buf)
}

// Ctx contains all the important data. while you can easily access
// data in this struct from anwyehre, only do so via channels
type Ctx struct {
	*Hub
	CaretPosition
	FilterQuery
	enableSep           bool
	result              []Match
	mutex               sync.Mutex
	currentLine         int
	currentPage         *PageInfo
	maxPage             int
	selection           Selection
	lines               []Match
	current             []Match
	bufferSize          int
	config              *Config
	Matchers            []Matcher
	currentMatcher      int
	exitStatus          int
	selectionRangeStart int
	layoutType          string

	wait *sync.WaitGroup
}

func NewCtx(o CtxOptions) *Ctx {
	c := &Ctx{
		Hub:                 NewHub(),
		CaretPosition:       0,
		FilterQuery:         FilterQuery{},
		result:              []Match{},
		mutex:               sync.Mutex{},
		currentPage:         &PageInfo{0, 1, 0},
		maxPage:             0,
		selection:           Selection([]int{}),
		lines:               []Match{},
		current:             nil,
		config:              NewConfig(),
		Matchers:            nil,
		currentMatcher:      0,
		exitStatus:          0,
		selectionRangeStart: invalidSelectionRange,
		wait:                &sync.WaitGroup{},
	}

	if o != nil {
		// XXX Pray this is really nil :)
		c.enableSep = o.EnableNullSep()
		c.currentLine = o.InitialIndex()
		c.bufferSize = o.BufferSize()
		c.layoutType = o.LayoutType()
	}

	c.Matchers = []Matcher{
		NewIgnoreCaseMatcher(c.enableSep),
		NewCaseSensitiveMatcher(c.enableSep),
		NewRegexpMatcher(c.enableSep),
	}

	return c
}

const invalidSelectionRange = -1

func (c *Ctx) ReadConfig(file string) error {
	if err := c.config.ReadFilename(file); err != nil {
		return err
	}

	if err := c.LoadCustomMatcher(); err != nil {
		return err
	}

	if c.config.Matcher != "" {
		fmt.Fprintln(os.Stderr, "'Matcher' option in config file is deprecated. Use InitialMatcher instead")
		c.config.InitialMatcher = c.config.Matcher
	}

	c.SetCurrentMatcher(c.config.InitialMatcher)

	if c.layoutType == "" { // Not set yet
		if c.config.Layout != "" {
			c.layoutType = c.config.Layout
		}
	}

	return nil
}

func (c *Ctx) IsBufferOverflowing() bool {
	if c.bufferSize <= 0 {
		return false
	}

	return len(c.lines) > c.bufferSize
}

func (c *Ctx) IsRangeMode() bool {
	return c.selectionRangeStart != invalidSelectionRange
}

func (c *Ctx) SelectedRange() Selection {
	if !c.IsRangeMode() {
		return Selection{}
	}

	selectedLines := []int{}
	if c.selectionRangeStart < c.currentLine {
		for i := c.selectionRangeStart; i < c.currentLine; i++ {
			selectedLines = append(selectedLines, i)
		}
	} else {
		for i := c.selectionRangeStart; i > c.currentLine; i-- {
			selectedLines = append(selectedLines, i)
		}
	}
	return Selection(selectedLines)
}

func (c *Ctx) Result() []Match {
	return c.result
}

func (c *Ctx) AddWaitGroup(v int) {
	c.wait.Add(v)
}

func (c *Ctx) ReleaseWaitGroup() {
	c.wait.Done()
}

func (c *Ctx) WaitDone() {
	c.wait.Wait()
}

func (c *Ctx) ExecQuery() bool {
	if c.QueryLen() > 0 {
		c.SendQuery(c.Query().String())
		return true
	}
	return false
}

func (c *Ctx) DrawMatches(m []Match) {
	c.SendDraw(m)
}
func (c *Ctx) Refresh() {
	c.DrawMatches(nil)
}

func (c *Ctx) Buffer() []Match {
	// Copy lines so it's safe to read it
	lcopy := make([]Match, len(c.lines))
	copy(lcopy, c.lines)
	return lcopy
}

func (c *Ctx) NewBufferReader(r io.ReadCloser) *BufferReader {
	return &BufferReader{c, r, make(chan struct{})}
}

func (c *Ctx) NewView() *View {
	var layout Layout
	switch c.layoutType {
	case "bottom-up":
		layout = NewBottomUpLayout(c)
	default:
		layout = NewDefaultLayout(c)
	}
	return &View{c, layout}
}

func (c *Ctx) NewFilter() *Filter {
	return &Filter{c, make(chan string)}
}

func (c *Ctx) NewInput() *Input {
	// Create a new keymap object
	k := NewKeymap(c.config.Keymap, c.config.Action)
	k.ApplyKeybinding()
	return &Input{c, &sync.Mutex{}, nil, k, []string{}}
}

func (c *Ctx) SetQuery(q []rune) {
	c.FilterQuery = FilterQuery(q)
	c.SetCaretPos(c.QueryLen())
}

func (c *Ctx) Matcher() Matcher {
	return c.Matchers[c.currentMatcher]
}

func (c *Ctx) AddMatcher(m Matcher) error {
	if err := m.Verify(); err != nil {
		return fmt.Errorf("verification for custom matcher failed: %s", err)
	}
	c.Matchers = append(c.Matchers, m)
	return nil
}

func (c *Ctx) SetCurrentMatcher(n string) bool {
	for i, m := range c.Matchers {
		if m.String() == n {
			c.currentMatcher = i
			return true
		}
	}
	return false
}

func (c *Ctx) LoadCustomMatcher() error {
	if len(c.config.CustomMatcher) == 0 {
		return nil
	}

	for name, args := range c.config.CustomMatcher {
		if err := c.AddMatcher(NewCustomMatcher(c.enableSep, name, args)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Ctx) ExitWith(i int) {
	c.exitStatus = i
	c.Stop()
}

type signalHandler struct {
	*Ctx
	sigCh chan os.Signal
}

func (c *Ctx) NewSignalHandler() *signalHandler {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	return &signalHandler{c, sigCh}
}

func (s *signalHandler) Loop() {
	defer s.ReleaseWaitGroup()

	for {
		select {
		case <-s.LoopCh():
			return
		case <-s.sigCh:
			// XXX For future reference: DO NOT, and I mean DO NOT call
			// termbox.Close() here. Calling termbox.Close() twice in our
			// context actually BLOCKS. Can you believe it? IT BLOCKS.
			//
			// So if we called termbox.Close() here, and then in main()
			// defer termbox.Close() blocks. Not cool.
			s.ExitWith(1)
			return
		}
	}
}

func (c *Ctx) SetPrompt(p string) {
	c.config.Prompt = p
}

// RotateMatcher rotates the matchers
func (c *Ctx) RotateMatcher() {
	c.currentMatcher++
	if c.currentMatcher >= len(c.Matchers) {
		c.currentMatcher = 0
	}
}

// ExitStatus() returns the exit status that we think should be used
func (c Ctx) ExitStatus() int {
	return c.exitStatus
}
