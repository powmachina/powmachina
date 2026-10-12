package stratum

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"powmachina/internal/mh"
	"powmachina/internal/work"
)

// What a hostile server puts into the texts the miner prints: an escape sequence that clears the
// screen and moves the cursor up, and a line break followed by a line that looks like the miner's own.
var (
	esc    = string(rune(0x1b))
	hijack = esc + "[2J" + esc + "[1A" + "\r\n" + "BLOCK FOUND: lane C, height 1"
)

// logBuf collects the client's log.
type logBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

// scriptedServer accepts one connection after another and plays `script` on each: it gets the
// connection after the client's login line has been read.
func scriptedServer(t *testing.T, script func(c net.Conn, r *bufio.Reader)) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				if _, err := r.ReadBytes('\n'); err != nil {
					return
				}
				script(c, r)
			}()
		}
	}()
	return ln.Addr().String()
}

func sendLine(c net.Conn, v any) {
	b, _ := json.Marshal(v)
	c.Write(append(b, '\n'))
}

// assertPlainLog checks that the log is the lines the client wrote and nothing a terminal acts on.
func assertPlainLog(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		for _, r := range line {
			if r < 0x20 || r == 0x7f {
				t.Errorf("the log holds the control character %U: %q", r, line)
			}
		}
		if strings.HasPrefix(line, "BLOCK FOUND") {
			t.Errorf("the server wrote a line of its own into the log: %q", line)
		}
	}
}

// The network's name, the reason for a pause and the reason a share was refused are the server's
// text. The miner prints them, so what arrives has to come out as text: no escape sequences, and no
// line break that lets the server write a line that looks like the miner's own.
func TestTextFromTheServerIsPrintedAsText(t *testing.T) {
	addr := scriptedServer(t, func(c net.Conn, r *bufio.Reader) {
		sendLine(c, map[string]any{"id": 1, "result": LoginResult{ID: "s", Network: "regtest" + hijack}})
		sendLine(c, map[string]any{"method": "pause", "params": PauseParams{Lane: "C", Reason: "rotation" + hijack}})
		sendLine(c, map[string]any{"method": "job", "params": JobParams{JobID: "G1", Lane: "G", Height: 1,
			Header: strings.Repeat("00", mh.HeaderBytes), Target: strings.Repeat("ff", 32)}})
		// Every submit is refused, with a reason as long as a line may be.
		for {
			line, err := r.ReadBytes('\n')
			if err != nil {
				return
			}
			var req Request
			if json.Unmarshal(line, &req) != nil || req.ID == nil || req.Method != "submit" {
				continue
			}
			sendLine(c, Response{ID: *req.ID, Error: &Error{ErrCodeLowDiff, "low difficulty" + hijack + strings.Repeat("x", 4000)}})
		}
	})

	buf := &logBuf{}
	cl := &Client{URL: addr, Login: "x", Log: log.New(buf, "", 0)}
	jobs := make(chan *work.Job, 4)
	quit := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- cl.Run([]mh.Lane{mh.LaneC, mh.LaneG}, func(_ mh.Lane, j *work.Job) {
			if j != nil {
				jobs <- j
			}
		}, quit)
	}()
	var job *work.Job
	select {
	case job = <-jobs:
	case <-time.After(20 * time.Second):
		t.Fatalf("no job arrived\n%s", buf.String())
	}
	if ok, _, err := cl.Submit(job, 1); ok || err != nil {
		t.Fatalf("the refused share: accepted %v, err %v", ok, err)
	}
	close(quit)
	<-done

	text := buf.String()
	for _, want := range []string{"connected to", "regtest", "paused by server: rotation", "share rejected: low difficulty"} {
		if !strings.Contains(text, want) {
			t.Errorf("the log is missing %q:\n%q", want, text)
		}
	}
	assertPlainLog(t, text)
	if len(text) > 4*maxServerText+400 {
		t.Errorf("the log is %d bytes, the server's texts were not cut", len(text))
	}
}

// A refused login ends the miner with the server's reason in the message, which is printed too.
func TestLoginRefusalIsPrintedAsText(t *testing.T) {
	addr := scriptedServer(t, func(c net.Conn, r *bufio.Reader) {
		sendLine(c, Response{ID: 1, Error: &Error{ErrCodeBadLogin, "not an address" + hijack}})
	})
	buf := &logBuf{}
	cl := &Client{URL: addr, Login: "x", Log: log.New(buf, "", 0)}
	err := cl.Run([]mh.Lane{mh.LaneC}, func(mh.Lane, *work.Job) {}, make(chan struct{}))
	if err == nil || !strings.Contains(err.Error(), "not an address") {
		t.Fatalf("the refusal: %v", err)
	}
	assertPlainLog(t, err.Error())
	assertPlainLog(t, buf.String())
}

// A line that is not a message ends the session, and the reason goes to the log with whatever the
// line held.
func TestUnreadableLineFromTheServerIsPrintedAsText(t *testing.T) {
	addr := scriptedServer(t, func(c net.Conn, r *bufio.Reader) {
		c.Write([]byte(`{"id":1,"result":{}}` + "\n" + `{"method":"job","params":{"lane":"` + "G" + esc + `[2J","header":"","target":""}}` + "\n"))
		time.Sleep(200 * time.Millisecond)
	})
	buf := &logBuf{}
	cl := &Client{URL: addr, Login: "x", Log: log.New(buf, "", 0)}
	quit := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- cl.Run([]mh.Lane{mh.LaneG}, func(mh.Lane, *work.Job) {}, quit) }()
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(buf.String(), "reconnecting"); {
		if time.Now().After(deadline) {
			t.Fatalf("the session did not end:\n%q", buf.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(quit)
	<-done
	assertPlainLog(t, buf.String())
}
