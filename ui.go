package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/atotto/clipboard"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
)

type uiMode int

const (
	modeNormal uiMode = iota
	modeSearch
	modePicker
)

// level filter values, compared against Entry.LevelRank
const (
	lvlAll   = 0
	lvlWarns = 2
	lvlErrs  = 3
)

// messages — gen ties a message to one run of the child process, so output
// from a process killed by restart is ignored.
type linesMsg struct {
	gen     int
	entries []Entry
}
type srcClosedMsg struct{ gen int }
type procExitMsg struct {
	gen  int
	code int
	err  string
}
type clearFlashMsg struct{}

var (
	styHeader   = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("252")).Padding(0, 1)
	styHeaderHi = lipgloss.NewStyle().Background(lipgloss.Color("236")).Foreground(lipgloss.Color("214")).Bold(true)
	styHelp     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styHelpKey  = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	styPrompt   = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	styTime     = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styKey      = lipgloss.NewStyle().Foreground(lipgloss.Color("75"))
	styVal      = lipgloss.NewStyle()
	styMsg      = lipgloss.NewStyle().Foreground(lipgloss.Color("255"))
	styRawOut   = lipgloss.NewStyle()
	styRawErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	styGutOut   = lipgloss.NewStyle().Foreground(lipgloss.Color("238"))
	styGutErr   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	styExitOK   = lipgloss.NewStyle().Foreground(lipgloss.Color("78"))
	styExitBad  = lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true)
	styCursor   = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	styTicked   = lipgloss.NewStyle().Foreground(lipgloss.Color("78")).Bold(true)
	styUnticked = lipgloss.NewStyle().Foreground(lipgloss.Color("243"))
	styPickerTi = lipgloss.NewStyle().Foreground(lipgloss.Color("255")).Bold(true)
	styMatch    = lipgloss.NewStyle().Background(lipgloss.Color("220")).Foreground(lipgloss.Color("16"))
	stySelected = lipgloss.NewStyle().Background(lipgloss.Color("237")).Foreground(lipgloss.Color("231"))

	levelStyles = map[string]lipgloss.Style{
		"trace":   lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		"debug":   lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		"info":    lipgloss.NewStyle().Foreground(lipgloss.Color("78")),
		"warn":    lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		"warning": lipgloss.NewStyle().Foreground(lipgloss.Color("214")),
		"error":   lipgloss.NewStyle().Foreground(lipgloss.Color("203")).Bold(true),
		"fatal":   lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true),
		"panic":   lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true),
	}
)

func levelStyle(level string) lipgloss.Style {
	if s, ok := levelStyles[level]; ok {
		return s
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
}

type model struct {
	ch      <-chan Entry
	exitCh  <-chan procExitMsg
	kill    func()
	restart func() (<-chan Entry, <-chan procExitMsg, func())
	gen     int

	cmdline  string
	maxLines int

	entries  []Entry
	rendered []string // aligned with entries; "" = filtered out

	vp    viewport.Model
	input textinput.Model
	mode  uiMode

	search  string
	terms   []term
	needles []string // substrings to highlight in matching lines
	minLvl  int

	// field picker: every JSON key ever seen, and which ones are ticked.
	// An empty ticked set means "show every field".
	fields     []string
	fieldCount map[string]int
	jsonLines  int
	ticked     map[string]bool
	cursor     int

	// line selection
	selID    uint64 // Entry.ID, 0 = nothing selected
	expanded map[uint64]bool
	// visible-entry geometry, rebuilt by refreshContent
	visEntries []int // indices into entries
	visStart   []int // content line each visible entry starts on
	totalLines int

	follow   bool
	newSince int // visible lines arrived while not following
	pretty   bool
	flash    string

	width, height int
	ready         bool
	shown         int

	procDone bool
	exit     procExitMsg
}

func newModel(cmdline string, maxLines int, ch <-chan Entry, exitCh <-chan procExitMsg,
	kill func(), restart func() (<-chan Entry, <-chan procExitMsg, func())) model {
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "words, field=value, field>100…"
	return model{
		ch:         ch,
		exitCh:     exitCh,
		kill:       kill,
		restart:    restart,
		cmdline:    cmdline,
		maxLines:   maxLines,
		input:      ti,
		fieldCount: map[string]int{},
		ticked:     map[string]bool{},
		expanded:   map[uint64]bool{},
		follow:     true,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(waitForLines(m.ch, m.gen), waitForExit(m.exitCh, m.gen))
}

// waitForLines blocks for one entry then drains up to a batch without
// blocking, so a chatty process doesn't cause a render per line.
func waitForLines(ch <-chan Entry, gen int) tea.Cmd {
	return func() tea.Msg {
		e, ok := <-ch
		if !ok {
			return srcClosedMsg{gen: gen}
		}
		batch := linesMsg{gen: gen, entries: []Entry{e}}
		for len(batch.entries) < 500 {
			select {
			case e, ok := <-ch:
				if !ok {
					return batch
				}
				batch.entries = append(batch.entries, e)
			default:
				return batch
			}
		}
		return batch
	}
}

func waitForExit(ch <-chan procExitMsg, gen int) tea.Cmd {
	return func() tea.Msg {
		msg := <-ch
		msg.gen = gen
		return msg
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		bodyH := m.height - 2 // header + footer
		if !m.ready {
			m.vp = viewport.New(m.width, bodyH)
			m.vp.KeyMap = viewportKeys()
			m.ready = true
		} else {
			m.vp.Width, m.vp.Height = m.width, bodyH
		}
		m.input.Width = m.width - 12
		m.rebuildAll()
		return m, nil

	case linesMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		for _, e := range msg.entries {
			m.discoverFields(&e)
			m.entries = append(m.entries, e)
			r := m.renderIfVisible(&e)
			m.rendered = append(m.rendered, r)
			if r != "" && !m.follow {
				m.newSince++
			}
		}
		if over := len(m.entries) - m.maxLines; over > 0 {
			for i := range over {
				delete(m.expanded, m.entries[i].ID)
				if m.entries[i].ID == m.selID {
					m.selID = 0
				}
			}
			m.entries = m.entries[over:]
			m.rendered = m.rendered[over:]
		}
		m.refreshContent()
		return m, waitForLines(m.ch, m.gen)

	case srcClosedMsg:
		return m, nil

	case procExitMsg:
		if msg.gen != m.gen {
			return m, nil
		}
		m.procDone = true
		m.exit = msg
		return m, nil

	case clearFlashMsg:
		m.flash = ""
		return m, nil

	case tea.KeyMsg:
		switch m.mode {
		case modeSearch:
			return m.updateSearch(msg)
		case modePicker:
			return m.updatePicker(msg)
		}
		return m.updateNormal(msg)
	}

	var cmd tea.Cmd
	atBottom := m.vp.AtBottom()
	m.vp, cmd = m.vp.Update(msg)
	if atBottom != m.vp.AtBottom() {
		m.setFollow(m.vp.AtBottom())
	}
	return m, cmd
}

// discoverFields records JSON keys and how often they appear, so the picker
// can list every field that ever showed up (with frequencies).
func (m *model) discoverFields(e *Entry) {
	if e.JSON == nil {
		return
	}
	m.jsonLines++
	changed := false
	for k := range e.JSON {
		m.fieldCount[k]++
		if !slices.Contains(m.fields, k) {
			m.fields = append(m.fields, k)
			changed = true
		}
	}
	if changed {
		sortKeys(m.fields)
	}
}

func (m *model) setFollow(on bool) {
	m.follow = on
	if on {
		m.newSince = 0
		m.selID = 0
	}
}

func (m model) updateNormal(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		if m.kill != nil {
			m.kill()
		}
		return m, tea.Quit

	case "/":
		m.mode = modeSearch
		m.input.SetValue(m.search)
		m.input.CursorEnd()
		m.input.Focus()
		return m, textinput.Blink

	case "k":
		m.mode = modePicker
		m.cursor = 0
		return m, nil

	case "up":
		m.moveSel(-1)
		return m, nil

	case "down":
		m.moveSel(1)
		return m, nil

	case "enter":
		if m.selID != 0 {
			m.expanded[m.selID] = !m.expanded[m.selID]
			if idx := m.entryIndex(m.selID); idx >= 0 {
				m.rendered[idx] = m.renderIfVisible(&m.entries[idx])
			}
			m.refreshContent()
			m.ensureSelVisible()
		}
		return m, nil

	case "y":
		if idx := m.entryIndex(m.selID); idx >= 0 {
			if clipboard.WriteAll(m.entries[idx].Raw) == nil {
				m.flash = "copied!"
			} else {
				m.flash = "copy failed"
			}
			return m, tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return clearFlashMsg{} })
		}
		return m, nil

	case "e":
		m.minLvl = toggleLevel(m.minLvl, lvlErrs)
		m.rebuildAll()
		return m, nil

	case "w":
		m.minLvl = toggleLevel(m.minLvl, lvlWarns)
		m.rebuildAll()
		return m, nil

	case "j":
		m.pretty = !m.pretty
		m.rebuildAll()
		return m, nil

	case "f":
		m.setFollow(!m.follow)
		if m.follow {
			m.vp.GotoBottom()
		}
		return m, nil

	case "r":
		if m.restart == nil {
			return m, nil
		}
		if m.kill != nil {
			m.kill()
		}
		m.gen++
		m.ch, m.exitCh, m.kill = m.restart()
		m.entries = m.entries[:0]
		m.rendered = m.rendered[:0]
		m.expanded = map[uint64]bool{}
		m.selID = 0
		m.procDone = false
		m.setFollow(true)
		m.flash = "restarted"
		m.refreshContent()
		return m, tea.Batch(
			waitForLines(m.ch, m.gen),
			waitForExit(m.exitCh, m.gen),
			tea.Tick(1500*time.Millisecond, func(time.Time) tea.Msg { return clearFlashMsg{} }),
		)

	case "c":
		m.entries = m.entries[:0]
		m.rendered = m.rendered[:0]
		m.expanded = map[uint64]bool{}
		m.selID = 0
		m.newSince = 0
		m.refreshContent()
		return m, nil

	case "esc":
		switch {
		case m.selID != 0:
			m.selID = 0
			m.refreshContent()
		case m.search != "":
			m.search = ""
			m.terms = nil
			m.needles = nil
			m.rebuildAll()
		case m.minLvl != lvlAll:
			m.minLvl = lvlAll
			m.rebuildAll()
		}
		return m, nil

	case "g", "home":
		m.setFollow(false)
		m.vp.GotoTop()
		return m, nil

	case "G", "end":
		m.setFollow(true)
		m.vp.GotoBottom()
		return m, nil
	}

	var cmd tea.Cmd
	atBottom := m.vp.AtBottom()
	m.vp, cmd = m.vp.Update(msg)
	// scrolling up pauses follow; landing back on the bottom resumes it
	if atBottom != m.vp.AtBottom() {
		m.setFollow(m.vp.AtBottom())
	}
	return m, cmd
}

func toggleLevel(cur, want int) int {
	if cur == want {
		return lvlAll
	}
	return want
}

func (m model) updateSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		m.mode = modeNormal
		m.input.Blur()
		return m, nil
	case "esc":
		m.search = ""
		m.terms = nil
		m.needles = nil
		m.input.SetValue("")
		m.mode = modeNormal
		m.input.Blur()
		m.rebuildAll()
		return m, nil
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	if v := strings.ToLower(m.input.Value()); v != m.search {
		m.search = v
		m.terms = parseSearch(v)
		m.needles = m.needles[:0]
		for _, t := range m.terms {
			if t.op == opEq && t.val != "" {
				m.needles = append(m.needles, t.val)
			}
		}
		m.rebuildAll()
	}
	return m, cmd
}

func (m model) updatePicker(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter", "esc", "k", "q":
		m.mode = modeNormal
		m.rebuildAll()
		return m, nil

	case "up":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down":
		if m.cursor < len(m.fields)-1 {
			m.cursor++
		}

	case " ", "space":
		if m.cursor < len(m.fields) {
			k := m.fields[m.cursor]
			if m.ticked[k] {
				delete(m.ticked, k)
			} else {
				m.ticked[k] = true
			}
		}

	case "a":
		// everything off = show all fields again
		m.ticked = map[string]bool{}
	}
	return m, nil
}

func viewportKeys() viewport.KeyMap {
	k := viewport.DefaultKeyMap()
	// letters and arrows are taken by app bindings; keep page keys
	k.Up.SetKeys()
	k.Down.SetKeys()
	k.PageUp.SetKeys("pgup")
	k.PageDown.SetKeys("pgdown", "space")
	k.HalfPageUp.SetKeys("ctrl+u")
	k.HalfPageDown.SetKeys("ctrl+d")
	return k
}

// --- selection -------------------------------------------------------------

func (m *model) entryIndex(id uint64) int {
	if id == 0 {
		return -1
	}
	for i := range m.entries {
		if m.entries[i].ID == id {
			return i
		}
	}
	return -1
}

// selPos returns the selected entry's position among visible entries.
func (m *model) selPos() int {
	if m.selID == 0 {
		return -1
	}
	for p, idx := range m.visEntries {
		if m.entries[idx].ID == m.selID {
			return p
		}
	}
	return -1
}

func (m *model) moveSel(delta int) {
	n := len(m.visEntries)
	if n == 0 {
		return
	}
	pos := m.selPos()
	if pos < 0 {
		// start from the last line on screen
		pos = n - 1
		for p := n - 1; p >= 0; p-- {
			if m.visStart[p] < m.vp.YOffset+m.vp.Height {
				pos = p
				break
			}
		}
	} else {
		pos = max(0, min(n-1, pos+delta))
	}
	m.follow = false
	m.selID = m.entries[m.visEntries[pos]].ID
	m.refreshContent()
	m.ensureSelVisible()
}

func (m *model) ensureSelVisible() {
	pos := m.selPos()
	if pos < 0 {
		return
	}
	start := m.visStart[pos]
	end := m.totalLines
	if pos+1 < len(m.visStart) {
		end = m.visStart[pos+1]
	}
	end-- // last line of the entry
	if start < m.vp.YOffset {
		m.vp.SetYOffset(start)
	} else if end >= m.vp.YOffset+m.vp.Height {
		m.vp.SetYOffset(end - m.vp.Height + 1)
	}
}

// selectedRender restyles an already-rendered entry as the selected line:
// colours are stripped so the highlight bar reads clearly.
func selectedRender(txt string) string {
	lines := strings.Split(txt, "\n")
	for i, ln := range lines {
		plain := strings.Replace(ansi.Strip(ln), "▎", "▶", 1)
		lines[i] = stySelected.Render(plain)
	}
	return strings.Join(lines, "\n")
}

// --- rendering ---------------------------------------------------------------

// selectedKeys returns the ticked fields in display order, or nil when
// nothing is ticked (= show everything).
func (m *model) selectedKeys() []string {
	if len(m.ticked) == 0 {
		return nil
	}
	out := make([]string, 0, len(m.ticked))
	for _, k := range m.fields {
		if m.ticked[k] {
			out = append(out, k)
		}
	}
	return out
}

// renderIfVisible renders an entry, or returns "" when it is filtered out.
func (m *model) renderIfVisible(e *Entry) string {
	if e.LevelRank() < m.minLvl {
		return ""
	}
	if !e.matches(m.terms) {
		return ""
	}
	return m.renderEntry(e)
}

// highlight renders s in base style with any needle occurrences highlighted.
func (m *model) highlight(s string, base lipgloss.Style) string {
	lower := strings.ToLower(s)
	if len(m.needles) == 0 || len(lower) != len(s) {
		return base.Render(s)
	}
	var b strings.Builder
	i := 0
	for i < len(s) {
		best, bl := -1, 0
		for _, n := range m.needles {
			if idx := strings.Index(lower[i:], n); idx >= 0 && (best == -1 || idx < best || (idx == best && len(n) > bl)) {
				best, bl = idx, len(n)
			}
		}
		if best < 0 {
			b.WriteString(base.Render(s[i:]))
			break
		}
		if best > 0 {
			b.WriteString(base.Render(s[i : i+best]))
		}
		b.WriteString(styMatch.Render(s[i+best : i+best+bl]))
		i += best + bl
	}
	return b.String()
}

func (m *model) renderEntry(e *Entry) string {
	gutter := styGutOut.Render("▎")
	if e.Source == SrcStderr {
		gutter = styGutErr.Render("▎")
	}

	if e.JSON == nil {
		base := styRawOut
		if e.Source == SrcStderr {
			base = styRawErr
		}
		return m.clip(gutter + m.highlight(e.Raw, base))
	}

	if m.pretty || m.expanded[e.ID] {
		lines := strings.Split(m.renderPretty(e), "\n")
		for i, ln := range lines {
			lines[i] = m.clip(gutter + " " + ln)
		}
		return strings.Join(lines, "\n")
	}

	keys := m.selectedKeys()
	if keys == nil {
		keys = e.orderedKeys()
	}
	lvl := e.Level()
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		v, ok := e.JSON[k]
		if !ok {
			continue
		}
		val := formatValue(v)
		switch {
		case isOneOf(k, timeKeys):
			parts = append(parts, m.highlight(val, styTime))
		case isOneOf(k, levelKeys):
			parts = append(parts, m.highlight(strings.ToUpper(val), levelStyle(lvl)))
		case isOneOf(k, msgKeys):
			parts = append(parts, m.highlight(val, styMsg))
		default:
			parts = append(parts, styKey.Render(k+"=")+m.highlight(val, styVal))
		}
	}
	if len(parts) == 0 {
		return m.clip(gutter + styTime.Render("(none of the picked fields)"))
	}
	return m.clip(gutter + strings.Join(parts, " "))
}

func (m *model) renderPretty(e *Entry) string {
	obj := e.JSON
	if keys := m.selectedKeys(); keys != nil {
		obj = map[string]any{}
		for _, k := range keys {
			if v, ok := e.JSON[k]; ok {
				obj[k] = v
			}
		}
	}
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		return e.Raw
	}
	return string(b)
}

func (m *model) clip(s string) string {
	if m.width > 0 {
		return ansi.Truncate(s, m.width, "…")
	}
	return s
}

func (m *model) rebuildAll() {
	m.rendered = m.rendered[:0]
	for i := range m.entries {
		m.rendered = append(m.rendered, m.renderIfVisible(&m.entries[i]))
	}
	m.refreshContent()
}

func (m *model) refreshContent() {
	if !m.ready {
		return
	}
	vis := make([]string, 0, len(m.rendered))
	m.visEntries = m.visEntries[:0]
	m.visStart = m.visStart[:0]
	line := 0
	for i, r := range m.rendered {
		if r == "" {
			continue
		}
		if m.entries[i].ID == m.selID {
			r = selectedRender(r)
		}
		m.visEntries = append(m.visEntries, i)
		m.visStart = append(m.visStart, line)
		line += strings.Count(r, "\n") + 1
		vis = append(vis, r)
	}
	m.totalLines = line
	m.shown = len(vis)
	m.vp.SetContent(strings.Join(vis, "\n"))
	if m.follow {
		m.vp.GotoBottom()
	}
}

// --- views -------------------------------------------------------------------

func (m model) View() string {
	if !m.ready {
		return "starting…"
	}
	body := m.vp.View()
	if m.mode == modePicker {
		body = m.pickerView()
	}
	return m.headerView() + "\n" + body + "\n" + m.footerView()
}

func (m model) pickerView() string {
	var b strings.Builder
	b.WriteString("\n  " + styPickerTi.Render("Which fields do you want to see?") + "\n")
	b.WriteString("  " + styHelp.Render("nothing ticked = show everything") + "\n\n")

	if len(m.fields) == 0 {
		b.WriteString("  " + styHelp.Render("no JSON fields seen yet — waiting for logs…") + "\n")
	}
	for i, k := range m.fields {
		cursor := "   "
		if i == m.cursor {
			cursor = styCursor.Render(" ▸ ")
		}
		box := styUnticked.Render("[ ]")
		name := k
		if m.ticked[k] {
			box = styTicked.Render("[✓]")
			name = styTicked.Render(k)
		}
		pct := ""
		if m.jsonLines > 0 {
			pct = styHelp.Render(fmt.Sprintf("  %d%%", 100*m.fieldCount[k]/m.jsonLines))
		}
		b.WriteString(cursor + box + " " + name + pct + "\n")
	}

	// pad to viewport height so header/footer stay put
	lines := strings.Count(b.String(), "\n")
	for i := lines; i < m.vp.Height-1; i++ {
		b.WriteString("\n")
	}
	out := b.String()
	return strings.TrimSuffix(out, "\n")
}

func (m model) headerView() string {
	status := styExitOK.Render("● running")
	if m.procDone {
		switch {
		case m.exit.err != "":
			status = styExitBad.Render("● " + m.exit.err)
		case m.exit.code == 0:
			status = styExitOK.Render("● finished")
		default:
			status = styExitBad.Render(fmt.Sprintf("● exited %d", m.exit.code))
		}
	}

	parts := []string{
		styHeaderHi.Render("mike"),
		m.cmdline,
		status,
		fmt.Sprintf("%d/%d lines", m.shown, len(m.entries)),
	}
	if m.search != "" {
		parts = append(parts, "search:"+styHeaderHi.Render(m.search))
	}
	if n := len(m.ticked); n > 0 {
		parts = append(parts, styHeaderHi.Render(fmt.Sprintf("%d fields picked", n)))
	}
	switch m.minLvl {
	case lvlErrs:
		parts = append(parts, styExitBad.Render("errors only"))
	case lvlWarns:
		parts = append(parts, styHeaderHi.Render("warnings & errors"))
	}
	if !m.follow {
		p := "⏸ paused"
		if m.newSince > 0 {
			p = fmt.Sprintf("⏸ +%d new ↓", m.newSince)
		}
		parts = append(parts, styHeaderHi.Render(p))
	}
	if m.flash != "" {
		parts = append(parts, styHeaderHi.Render(m.flash))
	}
	h := styHeader.Render(strings.Join(parts, "  │  "))
	return ansi.Truncate(h, m.width, "…")
}

func helpItem(key, label string) string {
	return styHelpKey.Render(key) + " " + styHelp.Render(label)
}

func (m model) footerView() string {
	switch m.mode {
	case modeSearch:
		return styPrompt.Render(" search> ") + m.input.View()
	case modePicker:
		help := strings.Join([]string{
			helpItem("↑↓", "move"),
			helpItem("space", "tick"),
			helpItem("a", "show all"),
			helpItem("enter", "done"),
		}, styHelp.Render("  ·  "))
		return " " + help
	}
	items := []string{
		helpItem("↑↓", "pick line"),
		helpItem("enter", "open"),
		helpItem("y", "copy"),
		helpItem("/", "search"),
		helpItem("k", "fields"),
		helpItem("e", "errors"),
		helpItem("w", "warns"),
		helpItem("f", "follow"),
	}
	if m.restart != nil {
		items = append(items, helpItem("r", "restart"))
	}
	items = append(items, helpItem("q", "quit"))
	return ansi.Truncate(" "+strings.Join(items, styHelp.Render(" · ")), m.width, "…")
}
