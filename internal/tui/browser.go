package tui

import tea "github.com/charmbracelet/bubbletea"

func (m browserModel) Init() tea.Cmd {
	return loadViewCmd(m.conn, m.view, m.listSort, m.viewRequestID)
}
