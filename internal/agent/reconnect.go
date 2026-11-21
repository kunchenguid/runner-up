package agent

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// An agent run reports to whoever holds its link: at first the coordinator's
// place runner, over the run's own input and output (through SSH on another
// machine). The place runner ends a run on purpose by sending the line
// "stop". Input that ends without it, or a hangup, means the link was lost,
// such as an SSH connection that dropped or that the machine's SSH server
// closed when the coordinator stopped answering its keepalive: the place and
// its job keep running, their output held, while the place runner reconnects
// with `agent attach`. Losing the link never stops the place. It ends as any
// place does: its runner ends after its job, or it is stopped on purpose -
// by "stop" from whoever holds the link, by SIGTERM or SIGINT, or by a later
// run of the same place asking this one to let go (askToLetGo).
//
// The attach reaches the run through a listener on this machine's loopback
// address, whose port and secret the run keeps in the place's folder, which
// only the agent's account may read. The run hands the attach the output it
// held and reports to it from then on, ending with the line exitMarker and
// its exit code. A run whose place ended while it had no link keeps its
// result for resultWait for the place runner to reconnect and collect it.

const (
	// linkFile, in the place's folder, holds the listener's port and secret.
	linkFile = "link"
	// stopWord, alone on a line, ends the run on purpose.
	stopWord   = "stop"
	attachWord = "attach"
	// StopLine is what a place runner sends a run to stop it.
	StopLine = stopWord + "\n"
	// exitMarker and the run's exit code end what the run tells an attach.
	exitMarker = "runner-up agent: exit "
	// maxHeld is the most output a run holds while it has no link; it keeps
	// the newest.
	maxHeld = 1 << 20
)

// resultWait is how long a run whose place ended while it had no link keeps
// its result for the place runner to reconnect. A place runner reconnects
// every few seconds for as long as it holds the place, so it only goes
// uncollected when the coordinator let the place go.
const resultWait = 10 * time.Minute

// session is a run's link to whoever it reports to. Its Write goes to the
// current link, or is held while there is none.
type session struct {
	// stopPlace stops the place on purpose.
	stopPlace func()
	notify    chan struct{}

	mu   sync.Mutex
	link io.Writer
	// conn is set when the link is an attach, which is told the exit code.
	conn    net.Conn
	id      int
	held    []byte
	stopped bool
	// cannotReconnect: the run could not open its listener, so nobody can
	// reconnect to it, and a lost link stops the place.
	cannotReconnect bool
	say             func(format string, v ...any)
}

func newSession(stopPlace func()) *session {
	s := &session{stopPlace: stopPlace, notify: make(chan struct{}, 1)}
	s.say = func(format string, v ...any) { fmt.Fprintf(s, "runner-up agent: "+format+"\n", v...) }
	return s
}

func (s *session) poke() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// Write never fails: output is for the coordinator's log, and a lost link
// must not stop the place or its clean-up.
func (s *session) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.writeLocked(b)
	return len(b), nil
}

func (s *session) writeLocked(b []byte) {
	if s.link != nil {
		s.link.Write(b)
		return
	}
	s.held = append(s.held, b...)
	if extra := len(s.held) - maxHeld; extra > 0 {
		s.held = s.held[extra:]
	}
}

// attach makes w the link, replacing any earlier one, and hands it the held
// output. conn is set when w is an attach. It returns the link's id.
func (s *session) attach(w io.Writer, conn net.Conn) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn != nil {
		s.conn.Close()
	}
	s.id++
	s.link, s.conn = w, conn
	if len(s.held) > 0 {
		w.Write(s.held)
		s.held = nil
	}
	s.poke()
	return s.id
}

// lose says link id is gone, unless a newer link replaced it. The place keeps
// running for the place runner to reconnect, unless nobody can.
func (s *session) lose(id int) {
	s.mu.Lock()
	if id != s.id || s.link == nil {
		s.mu.Unlock()
		return
	}
	s.link, s.conn = nil, nil
	if s.stopped {
		s.mu.Unlock()
		return
	}
	if !s.cannotReconnect {
		s.writeLocked([]byte("runner-up agent: lost the link to the coordinator; the place keeps running for it to reconnect\n"))
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	s.stop()
}

// noReconnect says nobody can reconnect to the run: from now on a lost link
// stops the place, as does one lost already.
func (s *session) noReconnect() {
	s.mu.Lock()
	s.cannotReconnect = true
	lost := s.link == nil && !s.stopped
	s.mu.Unlock()
	if lost {
		s.stop()
	}
}

// stop ends the run on purpose: nobody waits for its result.
func (s *session) stop() {
	s.mu.Lock()
	s.stopped = true
	s.mu.Unlock()
	s.stopPlace()
	s.poke()
}

// finish hands the run's exit code to its link. With no link, it waits for
// one for up to resultWait, unless the run was stopped on purpose or nobody
// can reconnect.
func (s *session) finish(code int) {
	var giveUp <-chan time.Time
	for {
		s.mu.Lock()
		if s.link != nil {
			if s.conn != nil {
				fmt.Fprintf(s.link, "%s%d\n", exitMarker, code)
				s.conn.Close()
			}
			s.link, s.conn = nil, nil
			s.mu.Unlock()
			return
		}
		if s.stopped || s.cannotReconnect {
			s.mu.Unlock()
			return
		}
		if giveUp == nil {
			s.writeLocked([]byte(fmt.Sprintf("runner-up agent: keeping the place's result for %s for the coordinator to reconnect\n", resultWait)))
			t := time.NewTimer(resultWait)
			defer t.Stop()
			giveUp = t.C
		}
		s.mu.Unlock()
		select {
		case <-s.notify:
		case <-giveUp:
			return
		}
	}
}

// readLink reads link id's lines: "stop" stops the run, and the end of its
// input, unless after a stop, loses the link.
func (s *session) readLink(id int, in io.Reader) {
	sc := bufio.NewScanner(in)
	for sc.Scan() {
		if strings.TrimSpace(sc.Text()) == stopWord {
			s.stop()
			io.Copy(io.Discard, in)
			return
		}
	}
	s.lose(id)
}

// listen opens the run's listener for attaches and records it in the
// place's folder. Its close removes the record.
func (s *session) listen(a *AgentRun) (func(), error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	path := filepath.Join(a.Dir(), linkFile)
	record := fmt.Sprintf("%d %s\n", l.Addr().(*net.TCPAddr).Port, hex.EncodeToString(secret))
	if err := os.WriteFile(path, []byte(record), 0o600); err != nil {
		l.Close()
		return nil, err
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.serve(conn, secret)
		}
	}()
	return func() {
		l.Close()
		os.Remove(path)
	}, nil
}

// serve answers one connection to the listener: an attach, or a later run
// of the same place asking this one to stop.
func (s *session) serve(conn net.Conn, secret []byte) {
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	key, word, _ := strings.Cut(strings.TrimSpace(line), " ")
	got, _ := hex.DecodeString(key)
	if err != nil || subtle.ConstantTimeCompare(got, secret) != 1 {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	switch word {
	case attachWord:
		id := s.attach(&deadlineWriter{conn}, conn)
		s.readLink(id, r)
	case stopWord:
		s.stop()
		conn.Close()
	default:
		conn.Close()
	}
}

// deadlineWriter closes a link that takes more than a minute to take a
// write, which loses it.
type deadlineWriter struct{ c net.Conn }

func (d *deadlineWriter) Write(b []byte) (int, error) {
	d.c.SetWriteDeadline(time.Now().Add(time.Minute))
	n, err := d.c.Write(b)
	if err != nil {
		d.c.Close()
	}
	return n, err
}

// dialRun connects to the run holding a's place and says word.
func dialRun(a *AgentRun, word string) (net.Conn, error) {
	data, err := os.ReadFile(filepath.Join(a.Dir(), linkFile))
	if err != nil {
		return nil, err
	}
	port, secret, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	if _, err := strconv.Atoi(port); !ok || err != nil {
		return nil, errors.New("unreadable link record")
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 10*time.Second)
	if err != nil {
		return nil, err
	}
	if _, err := io.WriteString(conn, secret+" "+word+"\n"); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// askToLetGo asks a run still holding a's place, waiting for a link that may
// never come back, to stop and let go of it.
func askToLetGo(a *AgentRun) {
	if conn, err := dialRun(a, stopWord); err == nil {
		conn.Close()
	}
}

// agentAttach is `agent attach`: it reconnects to the run holding a's place,
// relays the run's output to stdout and its input's "stop", and exits with
// the run's exit code. It exits ExitGone when no run holds the place, and
// ExitFailed when the run went without saying how it ended.
func agentAttach(a *AgentRun, in io.Reader, stdout io.Writer) int {
	conn, err := dialRun(a, attachWord)
	if err != nil {
		fmt.Fprintf(stdout, "runner-up agent: no run holds place %s to reconnect to\n", a.Place)
		return ExitGone
	}
	defer conn.Close()
	go func() {
		sc := bufio.NewScanner(in)
		for sc.Scan() {
			if strings.TrimSpace(sc.Text()) == stopWord {
				io.WriteString(conn, stopWord+"\n")
				io.Copy(io.Discard, in)
				return
			}
		}
		// This attach's own link was lost: so is the run's link to it.
		conn.Close()
	}()
	sc := bufio.NewScanner(conn)
	sc.Buffer(make([]byte, 64*1024), maxHeld)
	for sc.Scan() {
		line := sc.Text()
		if code, ok := strings.CutPrefix(line, exitMarker); ok {
			if n, err := strconv.Atoi(code); err == nil {
				return n
			}
		}
		fmt.Fprintln(stdout, line)
	}
	fmt.Fprintf(stdout, "runner-up agent: lost place %s's run before it ended\n", a.Place)
	return ExitFailed
}
