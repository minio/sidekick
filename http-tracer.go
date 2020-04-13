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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	ui "github.com/gizak/termui/v3"

	"github.com/gizak/termui/v3/widgets"

	"github.com/minio/minio/pkg/console"
	"github.com/minio/minio/pkg/handlers"
)

// recordRequest - records the first recLen bytes
// of a given io.Reader
type recordRequest struct {
	// Data source to record
	io.Reader
	// Response body should be logged
	logBody bool
	// Internal recording buffer
	buf bytes.Buffer
	// request headers
	headers http.Header
	// total bytes read including header size
	bytesRead int
}

const timeFormat = "15:04:05.000"

func (r *recordRequest) Read(p []byte) (n int, err error) {
	n, err = r.Reader.Read(p)
	r.bytesRead += n

	if r.logBody {
		r.buf.Write(p[:n])
	}
	if err != nil {
		return n, err
	}
	return n, err
}
func (r *recordRequest) Size() int {
	sz := r.bytesRead
	for k, v := range r.headers {
		sz += len(k) + len(v)
	}
	return sz
}

// ResponseWriter - is a wrapper to trap the http response status code.
type ResponseWriter struct {
	http.ResponseWriter
	StatusCode int
	// Response body should be logged
	LogBody         bool
	TimeToFirstByte time.Duration
	StartTime       time.Time
	// number of bytes written
	bytesWritten int
	// Internal recording buffer
	headers bytes.Buffer
	body    bytes.Buffer
	// Indicate if headers are written in the log
	headersLogged bool
}

// NewResponseWriter - returns a wrapped response writer to trap
// http status codes for auditiing purposes.
func NewResponseWriter(w http.ResponseWriter) *ResponseWriter {
	return &ResponseWriter{
		ResponseWriter: w,
		StatusCode:     http.StatusOK,
		StartTime:      time.Now().UTC(),
	}
}

func (lrw *ResponseWriter) Write(p []byte) (int, error) {
	n, err := lrw.ResponseWriter.Write(p)
	lrw.bytesWritten += n
	if lrw.TimeToFirstByte == 0 {
		lrw.TimeToFirstByte = time.Now().UTC().Sub(lrw.StartTime)
	}
	if !lrw.headersLogged {
		// We assume the response code to be '200 OK' when WriteHeader() is not called,
		// that way following Golang HTTP response behavior.
		lrw.writeHeaders(&lrw.headers, http.StatusOK, lrw.Header())
		lrw.headersLogged = true
	}
	if lrw.StatusCode >= http.StatusBadRequest || lrw.LogBody {
		// Always logging error responses.
		lrw.body.Write(p)
	}
	if err != nil {
		return n, err
	}
	return n, err
}

// Write the headers into the given buffer
func (lrw *ResponseWriter) writeHeaders(w io.Writer, statusCode int, headers http.Header) {
	n, _ := fmt.Fprintf(w, "%d %s\n", statusCode, http.StatusText(statusCode))
	lrw.bytesWritten += n
	for k, v := range headers {
		n, _ := fmt.Fprintf(w, "%s: %s\n", k, v[0])
		lrw.bytesWritten += n
	}
}

// BodyPlaceHolder returns a dummy body placeholder
var BodyPlaceHolder = []byte("<BODY>")

// Body - Return response body.
func (lrw *ResponseWriter) Body() []byte {
	// If there was an error response or body logging is enabled
	// then we return the body contents
	if lrw.StatusCode >= http.StatusBadRequest || lrw.LogBody {
		return lrw.body.Bytes()
	}
	// ... otherwise we return the <BODY> place holder
	return BodyPlaceHolder
}

// WriteHeader - writes http status code
func (lrw *ResponseWriter) WriteHeader(code int) {
	lrw.StatusCode = code
	if !lrw.headersLogged {
		lrw.writeHeaders(&lrw.headers, code, lrw.ResponseWriter.Header())
		lrw.headersLogged = true
	}
	lrw.ResponseWriter.WriteHeader(code)
}

// Flush - Calls the underlying Flush.
func (lrw *ResponseWriter) Flush() {
	lrw.ResponseWriter.(http.Flusher).Flush()
}

// Size - reutrns the number of bytes written
func (lrw *ResponseWriter) Size() int {
	return lrw.bytesWritten
}

// Return the bytes that were recorded.
func (r *recordRequest) Data() []byte {
	// If body logging is enabled then we return the actual body
	if r.logBody {
		return r.buf.Bytes()
	}
	// ... otherwise we return <BODY> placeholder
	return BodyPlaceHolder
}
func httpInternalTrace(req *http.Request, resp *http.Response, reqTime, respTime time.Time, backend *Backend) {
	ti := InternalTrace(req, resp, reqTime, respTime)
	displayTrace(ti, backend)
}

// InternalTrace returns trace for sidekick http requests
func InternalTrace(req *http.Request, resp *http.Response, reqTime, respTime time.Time) TraceInfo {
	t := TraceInfo{}
	t.NodeName = req.Host
	if host, _, err := net.SplitHostPort(t.NodeName); err == nil {
		t.NodeName = host
	}
	reqHeaders := req.Header.Clone()
	reqHeaders.Set("Host", req.Host)
	if len(req.TransferEncoding) == 0 {
		reqHeaders.Set("Content-Length", strconv.Itoa(int(req.ContentLength)))
	}
	for _, enc := range req.TransferEncoding {
		reqHeaders.Add("Transfer-Encoding", enc)
	}
	reqBodyRecorder := &recordRequest{Reader: req.Body, logBody: false, headers: reqHeaders}
	req.Body = ioutil.NopCloser(reqBodyRecorder)
	respBodyStr := "<BODY>"
	if resp.Body != nil {
		respBody, _ := ioutil.ReadAll(resp.Body)
		respBodyStr = string(respBody)
	}

	rq := traceRequestInfo{
		Time:     reqTime,
		Method:   req.Method,
		Path:     req.URL.Path,
		RawQuery: req.URL.RawQuery,
		Client:   handlers.GetSourceIP(req),
		Headers:  reqHeaders,
		Body:     string(reqBodyRecorder.Data()),
	}
	rs := traceResponseInfo{
		Time:       respTime,
		Headers:    resp.Header.Clone(),
		StatusCode: resp.StatusCode,
		Body:       respBodyStr,
	}
	if rs.StatusCode == 0 {
		rs.StatusCode = http.StatusOK
	}

	t.ReqInfo = rq
	t.RespInfo = rs

	t.CallStats = traceCallStats{
		Latency: rs.Time.Sub(rq.Time),
		Rx:      reqBodyRecorder.Size(),
		Tx:      len(rs.Body),
	}
	t.Type = TraceMsgType
	if globalDebugEnabled {
		t.Type = DebugMsgType
	}
	return t
}

// Trace gets trace of http request
func Trace(f http.HandlerFunc, logBody bool, w http.ResponseWriter, r *http.Request, endpoint string) TraceInfo {
	// Setup a http request body recorder
	reqHeaders := r.Header.Clone()
	reqHeaders.Set("Host", r.Host)
	if len(r.TransferEncoding) == 0 {
		reqHeaders.Set("Content-Length", strconv.Itoa(int(r.ContentLength)))
	}
	for _, enc := range r.TransferEncoding {
		reqHeaders.Add("Transfer-Encoding", enc)
	}

	var reqBodyRecorder *recordRequest
	t := TraceInfo{}
	reqBodyRecorder = &recordRequest{Reader: r.Body, logBody: logBody, headers: reqHeaders}
	r.Body = ioutil.NopCloser(reqBodyRecorder)
	t.NodeName = r.Host
	if host, _, err := net.SplitHostPort(t.NodeName); err == nil {
		t.NodeName = host
	}

	rw := NewResponseWriter(w)
	rw.LogBody = logBody
	f(rw, r)

	rq := traceRequestInfo{
		Time:     time.Now().UTC(),
		Method:   r.Method,
		Path:     r.URL.Path,
		RawQuery: r.URL.RawQuery,
		Client:   handlers.GetSourceIP(r),
		Headers:  reqHeaders,
		Body:     string(reqBodyRecorder.Data()),
	}
	rs := traceResponseInfo{
		Time:       time.Now().UTC(),
		Headers:    rw.Header().Clone(),
		StatusCode: rw.StatusCode,
		Body:       string(rw.Body()),
	}

	if rs.StatusCode == 0 {
		rs.StatusCode = http.StatusOK
	}

	t.ReqInfo = rq
	t.RespInfo = rs

	t.NodeName = endpoint
	t.CallStats = traceCallStats{
		Latency:         rs.Time.Sub(rw.StartTime),
		Rx:              reqBodyRecorder.Size(),
		Tx:              rw.Size(),
		TimeToFirstByte: rw.TimeToFirstByte,
	}
	t.Type = TraceMsgType
	if globalDebugEnabled {
		t.Type = DebugMsgType
	}
	return t
}

// Log only the headers.
func httpTraceHdrs(f http.HandlerFunc, w http.ResponseWriter, r *http.Request, backend *Backend) {
	trace := Trace(f, false, w, r, backend.endpoint)
	displayTrace(trace, backend)
}
func displayTrace(trace TraceInfo, backend *Backend) {
	st := shortTrace(trace)
	backend.updateCallStats(st)

	if globalQuietEnabled || !globalTraceEnabled {
		return
	}
	if globalJSONEnabled {
		if globalDebugEnabled {
			buf := &bytes.Buffer{}
			enc := json.NewEncoder(buf)
			// Disable escaping special chars to display XML tags correctly
			enc.SetEscapeHTML(false)
			if err := enc.Encode(trace); err != nil {
				console.Fatalln(err)
			}
			console.Println(strings.TrimSuffix(buf.String(), "\n"))
		} else {
			buf := &bytes.Buffer{}
			enc := json.NewEncoder(buf)
			// Disable escaping special chars to display XML tags correctly
			enc.SetEscapeHTML(false)
			if err := enc.Encode(st); err != nil {
				console.Fatalln(err)
			}
			console.Println(strings.TrimSuffix(buf.String(), "\n"))
		}
		return
	}
	if globalConsoleDisplay {
		if globalDebugEnabled {
			console.Println(trace)
			return
		}
		console.Println(st)
		return
	}
}

// TraceInfo - represents a trace record, additionally
// also reports errors if any while listening on trace.
type TraceInfo struct {
	Type      string            `json:"type"`
	NodeName  string            `json:"nodename"`
	ReqInfo   traceRequestInfo  `json:"request"`
	RespInfo  traceResponseInfo `json:"response"`
	CallStats traceCallStats    `json:"stats"`
}

// Short trace record
type shortTraceMsg struct {
	Type       string         `json:"type"`
	Host       string         `json:"host"`
	Time       time.Time      `json:"time"`
	Client     string         `json:"client"`
	CallStats  traceCallStats `json:"callStats"`
	StatusCode int            `json:"statusCode"`
	StatusMsg  string         `json:"statusMsg"`
	Method     string         `json:"method"`
	Path       string         `json:"path"`
	Query      string         `json:"query"`
}

func initTermTable(backends []*Backend) *widgets.Table {
	table := widgets.NewTable()
	table.TextStyle = ui.NewStyle(ui.ColorWhite)
	table.SetRect(0, 0, 140, 11)
	table.Rows = make([][]string, len(backends)+1)
	maxEndpointWidth := 0
	table.Rows[0] =
		[]string{"Host", "Status", "TotCalls", "TotFailures", "Rx", "Tx", "Cum Downtime", "Last Downtime", "Min Latency", "Max Latency"}
	for i, b := range backends {
		row := []string{
			b.endpoint,
			b.getServerStatus(),
			strconv.FormatInt(b.Stats.TotCalls, 10),
			strconv.FormatInt(b.Stats.TotCallFailures, 10),
			humanize.IBytes(uint64(b.Stats.Rx)),
			humanize.IBytes(uint64(b.Stats.Tx)),
			b.Stats.CumDowntime.Round(time.Microsecond).String(),
			b.Stats.LastDowntime.Round(time.Microsecond).String(),
			"",
			""}
		table.Rows[i+1] = row
		if len(b.endpoint) > maxEndpointWidth {
			maxEndpointWidth = len(b.endpoint)
		}
	}
	table.ColumnWidths = []int{maxEndpointWidth + 2, 7, 10, 12, 10, 10, 15, 15, 15, 15}
	table.FillRow = true
	table.BorderStyle = ui.NewStyle(ui.ColorGreen)

	table.RowSeparator = true
	ui.Render(table)
	return table
}
func termTrace(backends []*Backend, t *widgets.Table) {
	idx := 0
	t.Rows[idx] =
		[]string{"Host", "Status", "TotCalls", "TotFailures", "Rx", "Tx", "Cum Downtime", "Last Downtime", "Min Latency", "Max Latency"}
	idx++
	for _, b := range backends {
		b.Stats.Lock()
		defer b.Stats.Unlock()
		minLatency := ""
		maxLatency := ""
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

func (s shortTraceMsg) String() string {
	var b = &strings.Builder{}
	fmt.Fprintf(b, " %5s: ", TraceMsgType)
	fmt.Fprintf(b, "%s ", s.Time.Format(timeFormat))
	statusStr := console.Colorize("RespStatus", fmt.Sprintf("%d %s", s.StatusCode, s.StatusMsg))
	if s.StatusCode >= http.StatusBadRequest {
		statusStr = console.Colorize("ErrStatus", fmt.Sprintf("%d %s", s.StatusCode, s.StatusMsg))
	}
	fmt.Fprintf(b, "[%s] ", statusStr)
	fmt.Fprintf(b, " %s ", s.Host)

	fmt.Fprintf(b, "%s %s%s", s.Method, s.Path, s.Query)
	fmt.Fprintf(b, " %s ", s.Client)
	spaces := 15 - len(s.Client)
	fmt.Fprintf(b, "%*s", spaces, " ")
	fmt.Fprint(b, console.Colorize("HeaderValue", fmt.Sprintf("  %2s", s.CallStats.Latency.Round(time.Microsecond).String())))
	spaces = 12 - len(fmt.Sprintf("%2s", s.CallStats.Latency.Round(time.Microsecond)))
	fmt.Fprintf(b, "%*s", spaces, " ")
	fmt.Fprint(b, console.Colorize("Stat", fmt.Sprintf(" ↑ ")))
	fmt.Fprint(b, console.Colorize("HeaderValue", humanize.IBytes(uint64(s.CallStats.Rx))))
	fmt.Fprint(b, console.Colorize("Stat", fmt.Sprintf(" ↓ ")))
	fmt.Fprint(b, console.Colorize("HeaderValue", humanize.IBytes(uint64(s.CallStats.Tx))))
	return b.String()
}

func shortTrace(t TraceInfo) shortTraceMsg {
	s := shortTraceMsg{}
	s.Type = console.Colorize("TraceMsgType", t.Type)
	s.Time = t.ReqInfo.Time
	if host, ok := t.ReqInfo.Headers["Host"]; ok {
		s.Host = strings.Join(host, "")
	}
	s.Host = t.NodeName
	s.StatusCode = t.RespInfo.StatusCode
	s.StatusMsg = http.StatusText(t.RespInfo.StatusCode)
	cSlice := strings.Split(t.ReqInfo.Client, ":")
	s.Client = cSlice[0]
	s.CallStats.Latency = t.CallStats.Latency
	s.CallStats.Rx = t.CallStats.Rx
	s.CallStats.Tx = t.CallStats.Tx
	s.Path = t.ReqInfo.Path
	s.Query = t.ReqInfo.RawQuery
	s.Method = t.ReqInfo.Method
	return s
}
func (trc TraceInfo) String() string {
	var nodeNameStr string
	var b = &strings.Builder{}

	if trc.NodeName != "" {
		nodeNameStr = fmt.Sprintf("%s: %s ", console.Colorize("TraceMsgType", DebugMsgType), trc.NodeName)
	}

	ri := trc.ReqInfo
	rs := trc.RespInfo
	fmt.Fprintf(b, "%s%s", nodeNameStr, console.Colorize("Request", fmt.Sprintf("[REQUEST] ")))
	fmt.Fprintf(b, "%s\n", ri.Time.Format(timeFormat))
	fmt.Fprintf(b, "%s%s", nodeNameStr, console.Colorize("Method", fmt.Sprintf("%s %s", ri.Method, ri.Path)))
	if ri.RawQuery != "" {
		fmt.Fprintf(b, "?%s", ri.RawQuery)
	}
	fmt.Fprint(b, "\n")
	host, ok := ri.Headers["Host"]
	if ok {
		delete(ri.Headers, "Host")
	}
	hostStr := strings.Join(host, "")
	fmt.Fprintf(b, "%s%s", nodeNameStr, console.Colorize("Host", fmt.Sprintf("Host: %s\n", hostStr)))
	for k, v := range ri.Headers {
		fmt.Fprintf(b, "%s%s", nodeNameStr, console.Colorize("ReqHeaderKey",
			fmt.Sprintf("%s: ", k))+console.Colorize("HeaderValue", fmt.Sprintf("%s\n", strings.Join(v, ""))))
	}

	fmt.Fprintf(b, "%s%s", nodeNameStr, console.Colorize("Body", fmt.Sprintf("%s\n", string(ri.Body))))
	fmt.Fprintf(b, "%s%s", nodeNameStr, console.Colorize("Response", fmt.Sprintf("[RESPONSE] ")))
	fmt.Fprintf(b, "[%s] ", rs.Time.Format(timeFormat))
	fmt.Fprint(b, console.Colorize("Stat", fmt.Sprintf("[ Duration %2s  ↑ %s  ↓ %s ]\n", trc.CallStats.Latency.Round(time.Microsecond), humanize.IBytes(uint64(trc.CallStats.Rx)), humanize.IBytes(uint64(trc.CallStats.Tx)))))

	statusStr := console.Colorize("RespStatus", fmt.Sprintf("%d %s", rs.StatusCode, http.StatusText(rs.StatusCode)))
	if rs.StatusCode != http.StatusOK {
		statusStr = console.Colorize("ErrStatus", fmt.Sprintf("%d %s", rs.StatusCode, http.StatusText(rs.StatusCode)))
	}
	fmt.Fprintf(b, "%s%s\n", nodeNameStr, statusStr)

	for k, v := range rs.Headers {
		fmt.Fprintf(b, "%s%s", nodeNameStr, console.Colorize("RespHeaderKey",
			fmt.Sprintf("%s: ", k))+console.Colorize("HeaderValue", fmt.Sprintf("%s\n", strings.Join(v, ""))))
	}
	fmt.Fprintf(b, "%s%s\n", nodeNameStr, console.Colorize("Body", string(rs.Body)))
	fmt.Fprint(b, nodeNameStr)
	return b.String()
}

// traceCallStats records request stats
type traceCallStats struct {
	Rx              int           `json:"rx"`
	Tx              int           `json:"tx"`
	Latency         time.Duration `json:"latency"`
	TimeToFirstByte time.Duration `json:"timetofirstbyte"`
}

// traceRequestInfo represents trace of http request
type traceRequestInfo struct {
	Time     time.Time   `json:"time"`
	Method   string      `json:"method"`
	Path     string      `json:"path,omitempty"`
	RawQuery string      `json:"rawquery,omitempty"`
	Headers  http.Header `json:"headers,omitempty"`
	Body     string      `json:"body,omitempty"`
	Client   string      `json:"client"`
}

// traceResponseInfo represents trace of http request
type traceResponseInfo struct {
	Time       time.Time   `json:"time"`
	Headers    http.Header `json:"headers,omitempty"`
	Body       string      `json:"body,omitempty"`
	StatusCode int         `json:"statuscode,omitempty"`
}
