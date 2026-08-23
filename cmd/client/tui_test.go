package main

import (
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// keyMsg builds the bubbletea key message for a key name.
func keyMsg(name string) tea.KeyMsg {
	switch name {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(name)}
	}
}

func isQuit(t *testing.T, cmd tea.Cmd) bool {
	t.Helper()
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// The download screen advertised "q to quit" in its help line, but nothing
// handled key presses there, so only ctrl+c worked.
func TestQuitKeysOnDownloadScreen(t *testing.T) {
	for _, key := range []string{"q", "esc", "ctrl+c"} {
		t.Run(key, func(t *testing.T) {
			m := initialModel([20]byte{}, t.TempDir())
			m.screen = screenDownload

			_, cmd := m.Update(keyMsg(key))
			if !isQuit(t, cmd) {
				t.Fatalf("%q on the download screen did not quit", key)
			}
		})
	}
}

func TestMenuNavigation(t *testing.T) {
	m := initialModel([20]byte{}, t.TempDir())

	next, _ := m.Update(keyMsg("down"))
	m = next.(model)
	if m.menuSel != 1 {
		t.Fatalf("menuSel = %d, want 1", m.menuSel)
	}

	next, _ = m.Update(keyMsg("enter"))
	m = next.(model)
	if m.screen != screenInput {
		t.Fatalf("screen = %v, want screenInput", m.screen)
	}
	if m.inputMode != 1 {
		t.Fatalf("inputMode = %d, want 1 (magnet)", m.inputMode)
	}

	// esc goes back to the menu
	next, _ = m.Update(keyMsg("esc"))
	m = next.(model)
	if m.screen != screenMenu {
		t.Fatalf("screen = %v, want screenMenu", m.screen)
	}
}

// A bad path must surface as the error screen, not a panic.
func TestLoadTorrentFileError(t *testing.T) {
	msg := loadTorrentFile("/nonexistent/nope.torrent")()
	if _, ok := msg.(errMsg); !ok {
		t.Fatalf("got %T, want errMsg", msg)
	}
}

// Sizes were always printed in GB, so a 4 MB torrent showed "0.00 GB".
func TestHumanBytes(t *testing.T) {
	cases := map[int64]string{
		0:                  "0 B",
		512:                "512 B",
		1024:               "1.00 KB",
		4 * 1024 * 1024:    "4.00 MB",
		1536 * 1024:        "1.50 MB",
		2684354560:         "2.50 GB",
		1024 * 1024 * 1024: "1.00 GB",
	}
	for in, want := range cases {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// truncate sliced bytes, which cuts multi-byte characters in half, and panicked
// for max < 3.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 10); got != "short" {
		t.Errorf("got %q", got)
	}
	if got := truncate("abcdefghij", 8); got != "abcde..." {
		t.Errorf("got %q", got)
	}

	// Multi-byte input must stay valid UTF-8 and not panic.
	name := "日本語のトレントファイル名前"
	got := truncate(name, 6)
	if len([]rune(got)) != 6 {
		t.Errorf("truncate returned %d runes, want 6 (%q)", len([]rune(got)), got)
	}
	for _, r := range got {
		if r == '�' {
			t.Errorf("truncate produced invalid UTF-8: %q", got)
		}
	}

	for _, max := range []int{0, 1, 2, 3} {
		truncate("abcdef", max) // must not panic
	}
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		45 * time.Second:            "45s",
		90 * time.Second:            "1m 30s",
		2*time.Hour + 5*time.Minute: "2h 5m",
	}
	for in, want := range cases {
		if got := formatDuration(in); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", in, got, want)
		}
	}
}

// The peer ID must be unique per instance; it used to be a fixed string.
func TestGeneratePeerIDIsUnique(t *testing.T) {
	a, err := generatePeerID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generatePeerID()
	if err != nil {
		t.Fatal(err)
	}

	if a == b {
		t.Fatal("two peer IDs were identical")
	}
	if string(a[:8]) != "-GT0001-" {
		t.Errorf("prefix = %q", a[:8])
	}
	for _, c := range a[8:] {
		if c < '0' || c > 'z' {
			t.Errorf("peer ID contains a non-printable byte: %v", a)
			break
		}
	}
}
