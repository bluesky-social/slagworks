package cmd

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/bluesky-social/slagworks/internal/cli/clientauth"
)

const watchInterval = 3 * time.Second

// watchable wraps a list command to add a -w/--watch flag.
// When -w is active, it captures the command's output and re-runs it every 3s
// using a bubbletea alt-screen model (no flicker). The command's own RunE needs
// no knowledge of watch mode. A failed run is shown and retried on the next
// tick, except one that only a new login can fix
// (clientauth.ErrLoginRequired), which ends the watch with that error:
// retrying it would ask the identity provider every 3s, for as long as the
// watch stays open, to refresh a token it already refused.
func watchable(cmd *cobra.Command) *cobra.Command {
	var watch bool
	original := cmd.RunE
	cmd.RunE = func(c *cobra.Command, args []string) error {
		if !watch {
			return original(c, args)
		}
		fetch := func() (string, error) {
			var buf strings.Builder
			c.SetOut(&buf)
			defer c.SetOut(os.Stdout)
			if err := original(c, args); err != nil {
				return "", err
			}
			return buf.String(), nil
		}
		if !term.IsTerminal(int(os.Stdout.Fd())) {
			return watchPlainList(c.Context(), fetch)
		}
		m := newListWatchModel(fetch)
		p := tea.NewProgram(m, tea.WithContext(c.Context()))
		final, err := p.Run()
		if err != nil {
			return err
		}
		if fm, ok := final.(listWatchModel); ok && fm.signInErr != nil {
			return fm.signInErr
		}
		return nil
	}
	cmd.Flags().BoolVarP(&watch, "watch", "w", false, "refresh every 3s")
	return cmd
}

// watchPlainList is the non-TTY fallback: print header + content to stdout on each tick.
func watchPlainList(ctx interface{ Done() <-chan struct{} }, fetch func() (string, error)) error {
	for {
		content, err := fetch()
		if errors.Is(err, clientauth.ErrLoginRequired) {
			return err
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
		} else {
			fmt.Printf("--- %s ---\n%s\n", time.Now().Format("15:04:05"), content)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(watchInterval):
		}
	}
}

// --- Bubble Tea model ---

type listWatchModel struct {
	fetch     func() (string, error)
	content   string
	fetchErr  error
	signInErr error // a fetch error only a new login fixes; ends the watch
	updated   time.Time
}

type listFetchedMsg struct {
	content string
	at      time.Time
}
type listFetchErrMsg struct{ err error }
type listTickMsg struct{}

func newListWatchModel(fetch func() (string, error)) listWatchModel {
	return listWatchModel{fetch: fetch}
}

func (m listWatchModel) Init() tea.Cmd {
	return listDoFetch(m.fetch)
}

func (m listWatchModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" || msg.String() == "q" {
			return m, tea.Quit
		}
	case listFetchedMsg:
		m.content = msg.content
		m.updated = msg.at
		m.fetchErr = nil
		return m, tea.Tick(watchInterval, func(time.Time) tea.Msg { return listTickMsg{} })
	case listFetchErrMsg:
		if errors.Is(msg.err, clientauth.ErrLoginRequired) {
			m.signInErr = msg.err
			return m, tea.Quit
		}
		m.fetchErr = msg.err
		return m, tea.Tick(watchInterval, func(time.Time) tea.Msg { return listTickMsg{} })
	case listTickMsg:
		return m, listDoFetch(m.fetch)
	}
	return m, nil
}

func (m listWatchModel) View() tea.View {
	ts := "..."
	if !m.updated.IsZero() {
		ts = m.updated.Format("15:04:05")
	}
	header := fmt.Sprintf("Every %.0fs — %s  (q to quit)\n\n",
		watchInterval.Seconds(), ts)
	body := m.content
	if m.fetchErr != nil {
		body = fmt.Sprintf("error: %v\n", m.fetchErr)
	}
	v := tea.NewView(header + body)
	v.AltScreen = true
	return v
}

func listDoFetch(fetch func() (string, error)) tea.Cmd {
	return func() tea.Msg {
		content, err := fetch()
		if err != nil {
			return listFetchErrMsg{err}
		}
		return listFetchedMsg{content: content, at: time.Now()}
	}
}

// Ensure listWatchModel satisfies io.Writer for cobra SetOut calls.
var _ io.Writer = (*strings.Builder)(nil)
