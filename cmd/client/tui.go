package main

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/Vaivaswat2244/go-torrent/internal/bencode"
	"github.com/Vaivaswat2244/go-torrent/internal/dht"
	"github.com/Vaivaswat2244/go-torrent/internal/engine"
	"github.com/Vaivaswat2244/go-torrent/internal/magnet"
	"github.com/Vaivaswat2244/go-torrent/internal/metadata"
	"github.com/Vaivaswat2244/go-torrent/internal/torrentfile"
)

type screen int

const (
	screenMenu screen = iota
	screenInput
	screenFetching
	screenDownload
	screenDone
	screenError
)

// maxLogLines is how much of the engine's event stream we keep on screen.
const maxLogLines = 6

type tickMsg time.Time
type metadataReadyMsg struct{ tf *torrentfile.TorrentFile }
type errMsg struct{ err error }
type logMsg string

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

// waitForLog blocks on the engine's event channel so progress messages reach
// the UI instead of being printed over the alt-screen.
func waitForLog(t *engine.Torrent) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-t.Events()
		if !ok {
			return nil
		}
		return logMsg(msg)
	}
}

type model struct {
	screen    screen
	menuSel   int
	inputMode int

	textInput textinput.Model
	torrent   *engine.Torrent
	progress  progress.Model
	stats     engine.TorrentStats
	logs      []string

	// fetchCancel aborts an in-flight magnet metadata fetch.
	fetchCancel context.CancelFunc

	peerID    [20]byte
	outputDir string
	errText   string
	width     int
}

func initialModel(peerID [20]byte, outputDir string) model {
	ti := textinput.New()
	ti.CharLimit = 512
	ti.Width = 60

	p := progress.New(
		progress.WithDefaultGradient(),
		progress.WithWidth(56),
	)

	return model{
		screen:    screenMenu,
		textInput: ti,
		progress:  p,
		peerID:    peerID,
		outputDir: outputDir,
	}
}

func (m model) Init() tea.Cmd { return nil }

// quit shuts down any running work before exiting.
func (m model) quit() (tea.Model, tea.Cmd) {
	if m.torrent != nil {
		m.torrent.Stop()
	}
	if m.fetchCancel != nil {
		m.fetchCancel()
	}
	return m, tea.Quit
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		if key.String() == "ctrl+c" {
			return m.quit()
		}
	}
	if ws, ok := msg.(tea.WindowSizeMsg); ok {
		m.width = ws.Width
		m.progress.Width = ws.Width/2 - 8
		return m, nil
	}

	switch m.screen {
	case screenMenu:
		return m.updateMenu(msg)
	case screenInput:
		return m.updateInput(msg)
	case screenFetching:
		return m.updateFetching(msg)
	case screenDownload:
		return m.updateDownload(msg)
	case screenDone, screenError:
		if key, ok := msg.(tea.KeyMsg); ok {
			if key.String() == "q" || key.String() == "enter" {
				return m.quit()
			}
		}
	}
	return m, nil
}

func (m model) updateMenu(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "up", "k":
			if m.menuSel > 0 {
				m.menuSel--
			}
		case "down", "j":
			if m.menuSel < 1 {
				m.menuSel++
			}
		case "enter", " ":
			m.inputMode = m.menuSel
			m.screen = screenInput
			if m.menuSel == 0 {
				m.textInput.Placeholder = "/path/to/file.torrent"
				m.textInput.Prompt = "📄 Path: "
			} else {
				m.textInput.Placeholder = "magnet:?xt=urn:btih:..."
				m.textInput.Prompt = "🧲 Magnet: "
			}
			m.textInput.Focus()
			return m, textinput.Blink
		case "q":
			return m.quit()
		}
	}
	return m, nil
}

func (m model) updateInput(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok {
		switch key.String() {
		case "esc":
			m.screen = screenMenu
			m.textInput.Blur()
			return m, nil
		case "enter":
			val := strings.TrimSpace(m.textInput.Value())
			if val == "" {
				return m, nil
			}
			m.screen = screenFetching

			if m.inputMode == 0 {
				return m, loadTorrentFile(val)
			}

			ctx, cancel := context.WithCancel(context.Background())
			m.fetchCancel = cancel
			return m, fetchMagnetMetadata(ctx, val, m.peerID)
		}
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

func (m model) updateFetching(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// A DHT metadata fetch can take a minute; let the user back out.
		if msg.String() == "esc" || msg.String() == "q" {
			if m.fetchCancel != nil {
				m.fetchCancel()
				m.fetchCancel = nil
			}
			m.screen = screenMenu
			return m, nil
		}
	case metadataReadyMsg:
		return m.startDownload(msg.tf)
	case errMsg:
		m.screen = screenError
		m.errText = msg.err.Error()
		return m, nil
	}
	return m, nil
}

func (m model) updateDownload(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		// The help line advertised "q to quit" but nothing handled key presses
		// on this screen, so only ctrl+c worked.
		switch msg.String() {
		case "q", "esc":
			return m.quit()
		}
		return m, nil

	case logMsg:
		m.logs = append(m.logs, string(msg))
		if len(m.logs) > maxLogLines {
			m.logs = m.logs[len(m.logs)-maxLogLines:]
		}
		return m, waitForLog(m.torrent)

	case tickMsg:
		m.stats = m.torrent.GetStats()

		switch m.stats.Status {
		case engine.StatusSeeding:
			m.screen = screenDone
			return m, nil
		case engine.StatusError:
			m.screen = screenError
			m.errText = m.torrent.Err()
			return m, nil
		}

		cmd := m.progress.SetPercent(m.stats.Progress / 100.0)
		return m, tea.Batch(tick(), cmd)

	case progress.FrameMsg:
		pm, cmd := m.progress.Update(msg)
		m.progress = pm.(progress.Model)
		return m, cmd

	case errMsg:
		m.screen = screenError
		m.errText = msg.err.Error()
		return m, nil
	}
	return m, nil
}

// ── Commands ──────────────────────────────────────────────────────────────────

func loadTorrentFile(path string) tea.Cmd {
	return func() tea.Msg {
		tf, err := torrentfile.Open(path)
		if err != nil {
			return errMsg{err}
		}
		return metadataReadyMsg{tf}
	}
}

func fetchMagnetMetadata(ctx context.Context, uri string, peerID [20]byte) tea.Cmd {
	return func() tea.Msg {
		mag, err := magnet.Parse(uri)
		if err != nil {
			return errMsg{fmt.Errorf("invalid magnet link: %w", err)}
		}

		peerChan := make(chan torrentfile.Peer, 100)
		go dht.FindPeers(ctx, mag.InfoHash, peerChan, nil)

		tempTF := mag.ToTorrentFile()
		req := torrentfile.AnnounceReq{
			PeerID: peerID,
			Port:   listenPort,
			Left:   -1, // real size is unknown until metadata arrives
			Event:  torrentfile.EventStarted,
		}
		if req.Left < 0 {
			req.Left = 0
		}

		for _, trackerURL := range torrentfile.FilterSupportedTrackers(mag.Trackers) {
			go func(trackerURL string) {
				// AnnounceTo dispatches on scheme, so http:// trackers in the
				// magnet are actually usable.
				resp, err := tempTF.AnnounceTo(trackerURL, req)
				if err != nil {
					return
				}
				for _, p := range resp.Peers {
					select {
					case peerChan <- p:
					case <-ctx.Done():
						return
					default:
					}
				}
			}(trackerURL)
		}

		rawInfo, err := metadata.Fetch(ctx, mag.InfoHash, peerID, peerChan)
		if err != nil {
			return errMsg{fmt.Errorf("metadata fetch failed: %w", err)}
		}

		infoDictVal, err := bencode.Decode(rawInfo)
		if err != nil {
			return errMsg{fmt.Errorf("failed to decode metadata: %w", err)}
		}

		// Unchecked, this assertion panicked the whole program on a malformed
		// metadata response.
		infoDict, ok := infoDictVal.(map[string]bencode.Value)
		if !ok {
			return errMsg{fmt.Errorf("metadata is not a bencode dictionary")}
		}

		tf, err := torrentfile.ParseInfoDict(infoDict, mag.InfoHash)
		if err != nil {
			return errMsg{fmt.Errorf("failed to parse metadata: %w", err)}
		}

		// tf.Name is deliberately left as parsed. It used to be overwritten with
		// the magnet's "dn" parameter, so a magnet without one produced a folder
		// literally named Unknown_Magnet_Download.
		tf.Trackers = torrentfile.FilterSupportedTrackers(mag.Trackers)
		return metadataReadyMsg{tf}
	}
}

func (m model) startDownload(tf *torrentfile.TorrentFile) (tea.Model, tea.Cmd) {
	t, err := engine.NewTorrent(tf, m.outputDir)
	if err != nil {
		m.screen = screenError
		m.errText = err.Error()
		return m, nil
	}
	m.torrent = t
	m.screen = screenDownload
	t.Start(m.peerID, listenPort)
	return m, tea.Batch(tick(), waitForLog(t), m.progress.SetPercent(0))
}

// ── Views ─────────────────────────────────────────────────────────────────────

func (m model) View() string {
	switch m.screen {
	case screenMenu:
		return m.viewMenu()
	case screenInput:
		return m.viewInput()
	case screenFetching:
		return m.viewFetching()
	case screenDownload:
		return m.viewDownload()
	case screenDone:
		return m.viewDone()
	case screenError:
		return m.viewError()
	}
	return ""
}

func (m model) viewMenu() string {
	opts := []string{"📄  Torrent file (.torrent)", "🧲  Magnet link"}
	var rows string
	for i, opt := range opts {
		if i == m.menuSel {
			rows += selectedStyle.Render("▶  "+opt) + "\n"
		} else {
			rows += unselectedStyle.Render("   "+opt) + "\n"
		}
	}
	return titleStyle.Render("⚡ go-torrent") + "\n" +
		boxStyle.Render(rows) + "\n" +
		helpStyle.Render("↑/↓ · enter to select · q to quit")
}

func (m model) viewInput() string {
	var label string
	if m.inputMode == 0 {
		label = subtitleStyle.Render("Enter path to .torrent file")
	} else {
		label = subtitleStyle.Render("Paste magnet link")
	}
	return titleStyle.Render("⚡ go-torrent") + "\n" +
		boxStyle.Render(label+"\n\n"+m.textInput.View()) + "\n" +
		helpStyle.Render("enter to confirm · esc to go back")
}

func (m model) viewFetching() string {
	var content string
	if m.inputMode == 0 {
		content = subtitleStyle.Render("📄 Loading torrent file...")
	} else {
		content = subtitleStyle.Render("🔍 Fetching metadata from DHT...") +
			"\n\n" + dimStyle.Render("This may take up to 60 seconds.")
	}
	return titleStyle.Render("⚡ go-torrent") + "\n" +
		boxStyle.Render(content) + "\n" +
		helpStyle.Render("esc to cancel · ctrl+c to quit")
}

func (m model) viewDownload() string {
	s := m.stats

	row := func(label, val string) string {
		return labelStyle.Render(label) + valueStyle.Render(val)
	}

	sizeLine := fmt.Sprintf("%s / %s", humanBytes(s.Downloaded), humanBytes(s.Total))
	speedLine := humanBytes(int64(s.SpeedBps)) + "/s"

	etaLine := "--"
	if s.ETA > 0 {
		etaLine = formatDuration(s.ETA)
	}

	info := strings.Join([]string{
		row("Status:  ", statusColor(s.Status).Render(string(s.Status))),
		row("File:    ", truncate(s.Name, 38)),
		row("Size:    ", sizeLine),
		row("Speed:   ", speedLine),
		row("ETA:     ", etaLine),
	}, "\n")

	bar := "\n" + m.progress.View() + "\n" +
		dimStyle.Render(fmt.Sprintf("%.2f%%", s.Progress))

	leftPanel := boxStyle.Render(info + "\n" + bar)

	rightPanel := boxStyle.Render(
		subtitleStyle.Bold(true).Render("🌐 Network") + "\n\n" +
			valueStyle.Render(fmt.Sprintf("%d", s.PeersActive)) + "\n" +
			dimStyle.Render("connected peers"),
	)

	panels := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)

	out := titleStyle.Render("⚡ go-torrent") + "\n" + panels

	// Engine progress messages, which used to be printed straight to stdout.
	if len(m.logs) > 0 {
		var lines []string
		for _, l := range m.logs {
			lines = append(lines, dimStyle.Render("· "+truncate(l, 76)))
		}
		out += "\n" + boxStyle.Render(strings.Join(lines, "\n"))
	}

	return out + "\n" + helpStyle.Render("q to quit")
}

func (m model) viewDone() string {
	content := lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Bold(true).
		Render("✅ Download complete!") +
		"\n\n" + dimStyle.Render(m.stats.Name)
	return titleStyle.Render("⚡ go-torrent") + "\n" +
		boxStyle.Render(content) + "\n" +
		helpStyle.Render("enter or q to exit")
}

func (m model) viewError() string {
	content := lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true).
		Render("❌ Error") + "\n\n" + dimStyle.Render(m.errText)
	return titleStyle.Render("⚡ go-torrent") + "\n" +
		boxStyle.Render(content) + "\n" +
		helpStyle.Render("enter or q to exit")
}

// ── Styles ────────────────────────────────────────────────────────────────────

var (
	titleStyle = lipgloss.NewStyle().
			Bold(true).Foreground(lipgloss.Color("205")).
			MarginBottom(1).MarginLeft(2)

	subtitleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	labelStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).Width(10)

	valueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")).Bold(true)

	dimStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	boxStyle = lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color("238")).
			Padding(1, 2).MarginLeft(2).MarginTop(1)

	selectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("205")).Bold(true)

	unselectedStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	helpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241")).
			MarginLeft(2).MarginTop(1)
)

func statusColor(s engine.Status) lipgloss.Style {
	switch s {
	case engine.StatusDownloading:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("33")).Bold(true)
	case engine.StatusSeeding:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("82")).Bold(true)
	case engine.StatusError, engine.StatusStalled:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	}
}

// humanBytes scales to the right unit. Sizes were previously always printed in
// GB, so anything under a gigabyte showed as "0.00 GB".
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit && exp < 4; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTP"[exp])
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	mnt := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60

	if h > 0 {
		return fmt.Sprintf("%dh %dm", h, mnt)
	}
	if mnt > 0 {
		return fmt.Sprintf("%dm %ds", mnt, s)
	}
	return fmt.Sprintf("%ds", s)
}

// truncate trims by rune, so multi-byte names are not cut mid-character.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 3 {
		return string(r[:max])
	}
	return string(r[:max-3]) + "..."
}

func runTUI(peerID [20]byte, outputDir string) {
	m := initialModel(peerID, outputDir)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatalf("TUI error: %v", err)
	}
}
