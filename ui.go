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
// along with this program.  If not, see <http://www.gnu.org/licenses/>.package main

package main

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/fatih/color"
	ui "github.com/gizak/termui/v3"
	"github.com/gizak/termui/v3/widgets"
	"github.com/minio/minio/pkg/console"
)

func initTermTable(backends []*Backend) *widgets.Table {
	table := widgets.NewTable()
	table.TextStyle = ui.NewStyle(ui.ColorWhite)
	table.SetRect(0, 0, 140, 20)
	table.Rows = make([][]string, len(backends)+1)
	maxEndpointWidth := 0
	idx := 0
	table.Rows[idx] =
		[]string{"Host", "Status", "TotCalls", "TotFailures", "Rx", "Tx", "Tot Downtime", "Last Downtime", "Min Latency", "Max Latency"}
	idx++
	for _, b := range backends {
		row := []string{
			b.endpoint,
			b.getServerStatus(),
			strconv.FormatInt(b.Stats.TotCalls, 10),
			strconv.FormatInt(b.Stats.TotCallFailures, 10),
			humanize.IBytes(uint64(b.Stats.Rx)),
			humanize.IBytes(uint64(b.Stats.Tx)),
			b.Stats.CumDowntime.Round(time.Microsecond).String(),
			b.Stats.LastDowntime.Round(time.Microsecond).String(),
			"Calculating...",
			"Calculating..."}
		table.Rows[idx] = row
		if len(b.endpoint) > maxEndpointWidth {
			maxEndpointWidth = len(b.endpoint)
		}
		idx++
	}
	table.ColumnWidths = []int{maxEndpointWidth + 2, 7, 10, 12, 10, 10, 15, 15, 15, 15}
	table.FillRow = true
	table.BorderStyle = ui.NewStyle(ui.ColorGreen)
	table.RowSeparator = true
	ui.Render(table)
	return table
}

func termUI(backends []*Backend, t *widgets.Table) {
	idx := 0
	t.Rows[idx] =
		[]string{"Host", "Status", "TotCalls", "TotFailures", "Rx", "Tx", "Tot Downtime", "Last Downtime", "Min Latency", "Max Latency"}
	idx++
	for _, b := range backends {
		b.Stats.Lock()
		defer b.Stats.Unlock()
		minLatency := "Calculating..."
		maxLatency := "Calculating..."
		if b.Stats.MaxLatency > 0 {
			minLatency = fmt.Sprintf("%2s", b.Stats.MinLatency.Round(time.Microsecond))
			maxLatency = fmt.Sprintf("%2s", b.Stats.MaxLatency.Round(time.Microsecond))
		}
		row := []string{
			b.endpoint,
			b.getServerStatus(),
			strconv.FormatInt(b.Stats.TotCalls, 10),
			strconv.FormatInt(b.Stats.TotCallFailures, 10),
			humanize.IBytes(uint64(b.Stats.Rx)),
			humanize.IBytes(uint64(b.Stats.Tx)),
			b.Stats.CumDowntime.String(),
			b.Stats.LastDowntime.String(),
			minLatency,
			maxLatency}

		t.Rows[idx] = row
		idx++
	}
	ui.Render(t)
}

func initUI(backends []*Backend) {
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
		if err := ui.Init(); err != nil {
			console.Fatalln("failed to initialize termui: %v", err)
		}
		termTable := initTermTable(backends)

		go func(backends []*Backend) {
			tickerCount := 1
			uiEvents := ui.PollEvents()
			ticker := time.NewTicker(time.Second).C
			for {
				select {
				case e := <-uiEvents:
					switch e.ID {
					case "q", "<C-c>":
						ui.Clear()
						ui.Close()
						os.Exit(0)
					}
				case <-ticker:
					termUI(backends, termTable)
					ui.Render(termTable)
					tickerCount++
				}
			}
		}(backends)
	}

}
