package main

import (
	"net/url"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Vaivaswat2244/go-torrent/internal/engine"
	"github.com/Vaivaswat2244/go-torrent/internal/magnet"
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
			m := initialModel([20]byte{}, t.TempDir(), 6881, engine.Limits{})
			m.screen = screenDownload

			_, cmd := m.Update(keyMsg(key))
			if !isQuit(t, cmd) {
				t.Fatalf("%q on the download screen did not quit", key)
			}
		})
	}
}

func TestMenuNavigation(t *testing.T) {
	m := initialModel([20]byte{}, t.TempDir(), 6881, engine.Limits{})

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

// longMagnet builds a magnet of the shape public indexers hand out: an info
// hash, a display name, and a long tail of trackers.
func longMagnet() (uri string, trackers []string) {
	trackers = []string{
		"udp://tracker.opentrackr.org:1337/announce",
		"udp://open.demonii.com:1337/announce",
		"udp://open.stealth.si:80/announce",
		"udp://tracker.torrent.eu.org:451/announce",
		"udp://exodus.desync.com:6969/announce",
		"udp://tracker.moeking.me:6969/announce",
		"udp://explodie.org:6969/announce",
		"udp://tracker.dler.org:6969/announce",
		"udp://opentracker.i2p.rocks:6969/announce",
		"udp://tracker1.bt.moack.co.kr:80/announce",
		"udp://tracker.theoks.net:6969/announce",
		"udp://tracker.bittor.pw:1337/announce",
		"https://tracker.tamersunion.org:443/announce",
		"udp://tracker-udp.gbitt.info:80/announce",
		"http://tracker.openbittorrent.com:80/announce",
	}

	uri = "magnet:?xt=urn:btih:88594aaacbde40ef3e2510c47374ec0aa396c08e" +
		"&dn=" + url.QueryEscape("ubuntu-24.04.1-desktop-amd64.iso")
	for _, tr := range trackers {
		uri += "&tr=" + url.QueryEscape(tr)
	}
	return uri, trackers
}

// The magnet input used to cap at 512 characters, which silently cut the
// tracker list off real magnet links. The info hash survives, since it sits at
// the front, so the download still starts — it just quietly loses every tracker
// past the cutoff and falls back to DHT alone.
func TestLongMagnetLinkIsNotTruncated(t *testing.T) {
	uri, trackers := longMagnet()

	if len(uri) <= 512 {
		t.Fatalf("test magnet is only %d chars; it must exceed the old 512 limit to be meaningful", len(uri))
	}

	m := initialModel([20]byte{}, t.TempDir(), 6881, engine.Limits{})

	// Menu -> Magnet link -> input screen.
	next, _ := m.Update(keyMsg("down"))
	m = next.(model)
	next, _ = m.Update(keyMsg("enter"))
	m = next.(model)
	if m.screen != screenInput || m.inputMode != 1 {
		t.Fatalf("expected the magnet input screen, got screen=%v mode=%d", m.screen, m.inputMode)
	}

	// A paste arrives as one batch of runes.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(uri)})
	m = next.(model)

	got := m.textInput.Value()
	if got != uri {
		t.Fatalf("input was truncated: kept %d of %d characters", len(got), len(uri))
	}

	// What actually matters is that the trackers survive the round trip.
	mag, err := magnet.Parse(got)
	if err != nil {
		t.Fatalf("parsing the pasted magnet: %v", err)
	}
	if len(mag.Trackers) != len(trackers) {
		t.Errorf("kept %d of %d trackers", len(mag.Trackers), len(trackers))
	}
	for i, want := range trackers {
		if i < len(mag.Trackers) && mag.Trackers[i] != want {
			t.Errorf("tracker %d = %q, want %q", i, mag.Trackers[i], want)
		}
	}
}

// Long filesystem paths go through the same field.
func TestLongPathIsNotTruncated(t *testing.T) {
	path := "/home/user/" + strings.Repeat("a-fairly-long-directory-name/", 25) + "file.torrent"
	if len(path) <= 512 {
		t.Fatalf("test path is only %d chars", len(path))
	}

	m := initialModel([20]byte{}, t.TempDir(), 6881, engine.Limits{})

	next, _ := m.Update(keyMsg("enter")) // torrent-file mode
	m = next.(model)

	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(path)})
	m = next.(model)

	if got := m.textInput.Value(); got != path {
		t.Errorf("path truncated: kept %d of %d characters", len(got), len(path))
	}
}
