// Copyright (c) 2020 MinIO, Inc.
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	"github.com/gdamore/tcell/v2"
	"github.com/minio/pkg/console"
	"github.com/rivo/tview"
)

var (
	app *tview.Application
)

type nodesView struct {
	*tview.Table
	header []string
}

func initNodesTable() *nodesView {
	t := tview.NewTable().
		SetFixed(1, 0).
		SetSelectable(true, false).
		SetBorders(false).SetSeparator('|')
	t.SetTitle("Nodes")
	t.SetBorderAttributes(tcell.AttrDim)
	t.SetBorderPadding(1, 0, 1, 1)
	t.SetBackgroundColor(tcell.ColorDefault)
	t.SetBorderColor(tcell.ColorTeal)
	t.SetBordersColor(tcell.ColorTeal)

	header := []string{"SITE",
		"HOST",
		"STATUS",
		"CALLS",
		"FAILURES",
		"Rx",
		"Tx",
		"TOTAL DOWNTIME",
		"LAST DOWNTIME",
		"MIN LATENCY",
		"MAX LATENCY"}

	return &nodesView{t, header}
}

func initUI(m *multisite) {
	if globalConsoleDisplay {
		console.SetColor("LogMsgType", color.New(color.FgHiMagenta))
		console.SetColor("TraceMsgType", color.New(color.FgYellow))
		console.SetColor("Stat", color.New(color.FgYellow))
		console.SetColor("Request", color.New(color.FgCyan))
		console.SetColor("Method", color.New(color.Bold, color.FgWhite))
		console.SetColor("Host", color.New(color.Bold, color.FgGreen))
		console.SetColor("ReqHeaderKey", color.New(color.Bold, color.FgWhite))
		console.SetColor("RespHeaderKey", color.New(color.Bold, color.FgCyan))
		console.SetColor("RespStatus", color.New(color.Bold, color.FgYellow))
		console.SetColor("ErrStatus", color.New(color.Bold, color.FgRed))
		console.SetColor("Response", color.New(color.FgGreen))
	} else {
		grid := tview.NewGrid().SetRows(-6, -94).SetBorders(false)
		grid.SetBorderColor(tcell.ColorTeal)
		frame := tview.NewFrame(tview.NewBox()).
			SetBorders(1, 1, 1, 1, 1, 1).
			AddText("ｓｉｄｅｋｉｃｋ", true, tview.AlignCenter, tcell.ColorWhite).
			AddText("<ctrl-c> Quit", true, tview.AlignRight, tcell.ColorSlateGray)

		grid.AddItem(frame, 0, 0, 3, 1, 0, 100, false)
		nodesTable := initNodesTable()
		nodesTable.populate(m)
		grid.AddItem(nodesTable, 1, 0, 1, 1, 0, 0, true)
		go func() {
			for {
				time.Sleep(time.Millisecond * 500)
				app.QueueUpdateDraw(func() {
					nodesTable.populate(m)
				})
			}
		}()

		app = tview.NewApplication()
		app.SetBeforeDrawFunc(func(s tcell.Screen) bool {
			s.Clear()
			return false
		})
		app.SetInputCapture(func(event *tcell.EventKey) *tcell.EventKey {
			k := event.Key()
			if k == tcell.KeyCtrlC {
				app.Stop()
				os.Exit(0)
			}
			if k == tcell.KeyRune {
				switch event.Rune() {
				case 'q':
					app.Stop()
					os.Exit(0)
				}
			}
			return event
		})

		go func() {
			if err := app.SetRoot(grid, true).SetFocus(grid).Run(); err != nil {
				panic(err)
			}
		}()
		return
	}
}
func (n *nodesView) getStylizedCell(backends []*Backend, row, col int, header bool) *tview.TableCell {
	var cell *tview.TableCell
	maxEndpointWidth := 0
	if header {
		for _, b := range backends {
			if len(b.endpoint) > maxEndpointWidth {
				maxEndpointWidth = len(b.endpoint)
			}
		}
	}
	if row == 0 && header {
		cell = &tview.TableCell{
			Text:            strings.ToUpper(n.header[col]),
			Color:           tcell.ColorSilver,
			Align:           tview.AlignCenter,
			BackgroundColor: tcell.ColorTeal,
			NotSelectable:   true,
			Attributes:      tcell.AttrBold,
			Expansion:       1,
		}
		maxWidth := 0
		switch col {
		case 0:
			maxWidth = maxEndpointWidth + 2
		case 1:
			maxWidth = 7
		case 2:
			maxWidth = 10
		case 3:
			maxWidth = 12
		case 4 - 5:
			maxWidth = 10
		case 6 - 9:
			maxWidth = 15
		}
		cell.SetMaxWidth(maxWidth)
		return cell
	}
	// get style for remaining cells
	align := tview.AlignLeft
	if col >= 1 {
		align = tview.AlignRight
	}
	color := tcell.ColorWhite
	if col == 0 {
		color = tcell.ColorSilver
	}
	cell = &tview.TableCell{
		Color: color,
		Align: align,
	}

	return cell
}

func (n *nodesView) populate(m *multisite) int {
	n.Clear()
	var backends []*Backend
	for _, s := range m.sites {
		backends = append(backends, s.backends...)
	}
	rows := len(backends) + 1
	cols := len(n.header)

	var cell *tview.TableCell
	for c := 0; c < cols; c++ {
		cell = n.getStylizedCell(backends, 0, c, true)
		cell.SetText(n.header[c])
		n.SetCell(0, c, cell)
	}

	fixedRows := 1
	for r := 0; r < (rows - fixedRows); r++ {
		for c := 0; c < cols; c++ {
			cell = n.getStylizedCell(backends, r, c, false)
			b := backends[r]
			minLatency := "0s"
			maxLatency := "0s"
			if b.Stats.MaxLatency > 0 {
				minLatency = fmt.Sprintf("%2s", b.Stats.MinLatency.Round(time.Microsecond))
				maxLatency = fmt.Sprintf("%2s", b.Stats.MaxLatency.Round(time.Microsecond))
			}
			var text string
			switch c {
			case 0:
				text = humanize.Ordinal(b.siteNumber)
			case 1:
				text = b.endpoint
			case 2:
				text = b.getServerStatus()
				if text == "UP" {
					cell.SetTextColor(tcell.ColorGreenYellow)
				} else {
					cell.SetTextColor(tcell.ColorIndianRed)
				}
			case 3:
				text = strconv.FormatInt(b.Stats.TotCalls, 10)
			case 4:
				text = strconv.FormatInt(b.Stats.TotCallFailures, 10)
			case 5:
				text = humanize.IBytes(uint64(b.Stats.Rx))
			case 6:
				text = humanize.IBytes(uint64(b.Stats.Tx))
			case 7:
				text = b.Stats.CumDowntime.Round(time.Microsecond).String()
			case 8:
				text = b.Stats.LastDowntime.Round(time.Microsecond).String()
			case 9:
				text = minLatency
			case 10:
				text = maxLatency
			}
			cell.SetText(text)
			n.SetCell(r+fixedRows, c, cell)
		}
	}
	return rows
}
