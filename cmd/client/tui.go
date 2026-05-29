package main

import (
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

type tickMsg time.Time
type metadataReadyMsg struct{ tf *torrentfile.TorrentFile }
type errMsg struct{ err error }
type peerUpdateMsg struct {
	peers   []string
	seeders []string
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg {
		return tickMsg(t)
	})
}

type model struct {
	screen    screen
	menuSel   int
	inputMode int

	textInput textinput.Model
	torrent   *engine.Torrent
	progress  progress.Model
	stats     engine.TorrentStats

	// peer tracking
	connectedPeers   []string
	connectedSeeders []string

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

func (m model) Init() tea.Cmd {
	return nil
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	// Global handlers
	if key, ok := msg.(tea.KeyMsg); ok {
		if key.String() == "ctrl+c" {
			if m.torrent != nil {
				m.torrent.Stop()
			}
			return m, tea.Quit
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
				return m, tea.Quit
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
			return m, tea.Quit
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
				return m, m.loadTorrentFile(val)
			}
			return m, m.fetchMagnetMetadata(val)
		}
	}

	var cmd tea.Cmd
	m.textInput, cmd = m.textInput.Update(msg)
	return m, cmd
}

func (m model) updateFetching(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
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
	case tickMsg:
		m.stats = m.torrent.GetStats()
		if m.stats.Status == engine.StatusSeeding {
			m.screen = screenDone
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

func (m model) loadTorrentFile(path string) tea.Cmd {
	return func() tea.Msg {
		tf, err := torrentfile.Open(path)
		if err != nil {
			return errMsg{err}
		}
		return metadataReadyMsg{tf}
	}
}

func (m model) fetchMagnetMetadata(uri string) tea.Cmd {
	peerID := m.peerID
	return func() tea.Msg {
		mag, err := magnet.Parse(uri)
		if err != nil {
			return errMsg{fmt.Errorf("invalid magnet link: %w", err)}
		}

		peerChan := make(chan torrentfile.Peer, 100)
		go dht.FindPeers(mag.InfoHash, peerChan)

		tempTF := mag.ToTorrentFile()
		for _, trackerURL := range mag.Trackers {
			trackerURL := trackerURL
			go func() {
				peers, err := tempTF.RequestPeersUDP(trackerURL, peerID, 6881)
				if err != nil {
					return
				}
				for _, p := range peers {
					select {
					case peerChan <- p:
					default:
					}
				}
			}()
		}

		rawInfo, err := metadata.Fetch(mag.InfoHash, peerID, peerChan)
		if err != nil {
			return errMsg{fmt.Errorf("metadata fetch failed: %w", err)}
		}

		infoDictVal, err := bencode.Decode(rawInfo)
		if err != nil {
			return errMsg{fmt.Errorf("failed to decode metadata: %w", err)}
		}

		infoDict := infoDictVal.(map[string]bencode.Value)
		tf, err := torrentfile.ParseInfoDict(infoDict, mag.InfoHash)
		if err != nil {
			return errMsg{fmt.Errorf("failed to parse metadata: %w", err)}
		}

		tf.Name = mag.Name
		tf.Trackers = mag.Trackers
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
	t.Start(m.peerID, 6881)
	return m, tea.Batch(tick(), m.progress.SetPercent(0))
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
		helpStyle.Render("ctrl+c to quit")
}

func (m model) viewDownload() string {
	s := m.stats
	sizeTotal := float64(m.torrent.TF.Length) / 1024 / 1024 / 1024
	sizeDone := sizeTotal * (s.Progress / 100.0)

	// ── Left panel: info + progress ───────────────────────────────────────
	row := func(label, val string) string {
		return labelStyle.Render(label) + valueStyle.Render(val)
	}

	info := strings.Join([]string{
		row("Status:  ", statusColor(s.Status).Render(string(s.Status))),
		row("File:    ", truncate(s.Name, 38)),
		row("Size:    ", fmt.Sprintf("%.2f / %.2f GB", sizeDone, sizeTotal)),
		row("Speed:   ", fmt.Sprintf("%.2f MB/s", s.SpeedMBps)),
	}, "\n")

	bar := "\n" + m.progress.View() + "\n" +
		dimStyle.Render(fmt.Sprintf("%.2f%%", s.Progress))

	leftPanel := boxStyle.Render(info + "\n" + bar)

	// ── Right panel: peers + seeders ──────────────────────────────────────
	activePeers := s.PeersActive

	// We split active peers evenly into peers/seeders for display
	// (engine tracks total active; real seeder detection needs BEP work)
	peerLines := fmt.Sprintf("%s\n",
		valueStyle.Render(fmt.Sprintf("%d", activePeers)),
	)
	peerLines += dimStyle.Render("connected peers")

	rightPanel := boxStyle.Render(
		subtitleStyle.Bold(true).Render("🌐 Network") + "\n\n" +
			peerLines,
	)

	// ── Join panels side by side ──────────────────────────────────────────
	panels := lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, rightPanel)

	return titleStyle.Render("⚡ go-torrent") + "\n" +
		panels + "\n" +
		helpStyle.Render("q to quit")
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
	case engine.StatusError:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true)
	default:
		return lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true)
	}
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

func runTUI(peerID [20]byte, outputDir string) {
	m := initialModel(peerID, outputDir)
	p := tea.NewProgram(m, tea.WithAltScreen())
	if _, err := p.Run(); err != nil {
		log.Fatalf("TUI error: %v", err)
	}
}
