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
	"os"
	"time"

	"github.com/fatih/color"
	ui "github.com/gizak/termui/v3"
	"github.com/minio/minio/pkg/console"
)

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
		globalTermTable = initTermTable(backends)

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
					termTrace(backends, globalTermTable)
					ui.Render(globalTermTable)
					tickerCount++
				}
			}
		}(backends)
	}

}
