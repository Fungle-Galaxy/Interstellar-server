package main

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	pingIntervalV3 = 25 * time.Second
	pingTimeoutV3  = 5 * time.Second
	pingIntervalV4 = 25 * time.Second
	pingTimeoutV4  = 20 * time.Second
	maxPayloadV4   = 1000000
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  8192,
	WriteBufferSize: 8192,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// Session is one Engine.IO connection (transport: polling and/or websocket).
type Session struct {
	id  string
	eio int // 3 or 4

	pollMu sync.Mutex // one pending poll at a time

	qmu   sync.Mutex
	cond  *sync.Cond
	queue []string

	wsMu     sync.Mutex
	ws       *websocket.Conn
	upgraded bool

	closed   atomic.Bool
	lastSeen atomic.Int64

	hub    *Hub
	mgr    *Manager
	socket *Socket
}

func newSessionID() string {
	b := make([]byte, 15)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func (s *Session) pingInterval() time.Duration {
	if s.eio == 3 {
		return pingIntervalV3
	}
	return pingIntervalV4
}

func (s *Session) openPacket() string {
	if s.eio == 3 {
		return fmt.Sprintf(`0{"sid":%q,"upgrades":["websocket"],"pingInterval":25000,"pingTimeout":5000}`, s.id)
	}
	return fmt.Sprintf(`0{"sid":%q,"upgrades":["websocket"],"pingInterval":25000,"pingTimeout":20000,"maxPayload":%d}`, s.id, maxPayloadV4)
}

// send queues (polling) or writes (websocket) one engine.io packet.
func (s *Session) send(packet string) {
	if s.closed.Load() {
		return
	}
	s.wsMu.Lock()
	upgraded := s.upgraded && s.ws != nil
	s.wsMu.Unlock()
	if upgraded {
		s.writeWS(packet)
		return
	}
	s.qmu.Lock()
	s.queue = append(s.queue, packet)
	s.qmu.Unlock()
	s.cond.Signal()
}

func (s *Session) writeWS(packet string) {
	s.wsMu.Lock()
	defer s.wsMu.Unlock()
	if s.ws == nil {
		return
	}
	s.ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := s.ws.WriteMessage(websocket.TextMessage, []byte(packet)); err != nil {
		s.Close()
	}
}

// waitForPoll blocks until a packet is queued or the ping interval elapses.
// Returns the encoded payload; ok=false means timeout or closed.
func (s *Session) waitForPoll() (string, bool) {
	s.qmu.Lock()
	defer s.qmu.Unlock()
	if len(s.queue) > 0 {
		return s.popLocked(), true
	}
	if s.closed.Load() {
		return "", false
	}
	t := time.AfterFunc(s.pingInterval(), s.cond.Broadcast)
	s.cond.Wait()
	t.Stop()
	if len(s.queue) > 0 {
		return s.popLocked(), true
	}
	return "", false
}

func (s *Session) popLocked() string {
	packets := s.queue
	s.queue = nil
	if s.eio == 3 {
		var b strings.Builder
		for _, p := range packets {
			fmt.Fprintf(&b, "%d:%s", len(p), p)
		}
		return b.String()
	}
	return strings.Join(packets, "\x1e")
}

func decodePollPayload(eio int, body string) []string {
	if eio == 3 {
		var out []string
		for i := 0; i < len(body); {
			colon := strings.IndexByte(body[i:], ':')
			if colon < 0 || colon > 10 {
				break
			}
			n, err := strconv.Atoi(body[i : i+colon])
			if err != nil || n < 0 || i+colon+1+n > len(body) {
				break
			}
			out = append(out, body[i+colon+1:i+colon+1+n])
			i += colon + 1 + n
		}
		return out
	}
	return strings.Split(body, "\x1e")
}

// Close tears the session down and triggers hub cleanup exactly once.
func (s *Session) Close() {
	if s.closed.Swap(true) {
		return
	}
	s.cond.Broadcast()
	s.wsMu.Lock()
	if s.ws != nil {
		s.ws.Close()
	}
	s.wsMu.Unlock()
	s.mgr.RemoveSession(s.id)
}

// handleEnginePacket processes one inbound engine.io packet.
func (s *Session) handleEnginePacket(pkt string) {
	if pkt == "" || s.closed.Load() {
		return
	}
	switch pkt[0] {
	case '0': // open packet from client: invalid, ignore
	case '1': // close
		s.Close()
	case '2': // ping (EIO=3 clients ping us; reply pong)
		s.send("3")
	case '3': // pong (EIO=4, in reply to our ping)
	case '4': // message -> socket.io payload
		s.hub.Dispatch(s.socket, pkt[1:])
	case '5': // upgrade (only valid during ws probe)
	case '6': // noop
	}
}

func (s *Session) startKeepalive() {
	go func() {
		if s.eio == 4 {
			ticker := time.NewTicker(pingIntervalV4)
			defer ticker.Stop()
			watchdog := time.NewTicker(5 * time.Second)
			defer watchdog.Stop()
			limit := (pingIntervalV4 + pingTimeoutV4).Milliseconds()
			for {
				select {
				case <-ticker.C:
					s.send("2")
				case <-watchdog.C:
					if s.closed.Load() {
						return
					}
					if time.Since(time.UnixMilli(s.lastSeen.Load())) > time.Duration(limit)*time.Millisecond {
						s.Close()
						return
					}
				}
			}
		}
		// EIO=3: clients ping us; just watchdog.
		watchdog := time.NewTicker(5 * time.Second)
		defer watchdog.Stop()
		limit := (pingIntervalV3 + pingTimeoutV3).Milliseconds()
		for {
			<-watchdog.C
			if s.closed.Load() {
				return
			}
			if time.Since(time.UnixMilli(s.lastSeen.Load())) > time.Duration(limit)*time.Millisecond {
				s.Close()
				return
			}
		}
	}()
}

// Manager owns all sessions and routes socket.io packets to the Hub.
type Manager struct {
	mu       sync.Mutex
	sessions map[string]*Session
	hub      *Hub
}

func NewManager(hub *Hub) *Manager {
	return &Manager{sessions: make(map[string]*Session), hub: hub}
}

func (m *Manager) newSession(eio int) *Session {
	s := &Session{
		id:       newSessionID(),
		eio:      eio,
		cond:     sync.NewCond(&sync.Mutex{}),
		lastSeen: atomic.Int64{},
		hub:      m.hub,
		mgr:      m,
	}
	s.lastSeen.Store(time.Now().UnixMilli())
	s.socket = m.hub.CreateSocket(s)
	m.mu.Lock()
	m.sessions[s.id] = s
	m.mu.Unlock()
	// Handshake packet must be queued before any application events.
	s.qmu.Lock()
	s.queue = append([]string{s.openPacket()}, s.queue...)
	s.qmu.Unlock()
	// Now fire connect (queues clientPeerConfig after the open packet).
	m.hub.FireConnect(s.socket)
	s.startKeepalive()
	return s
}

func (m *Manager) get(id string) *Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[id]
}

// RemoveSession is called from Session.Close; performs hub cleanup.
func (m *Manager) RemoveSession(id string) {
	m.mu.Lock()
	sess, ok := m.sessions[id]
	delete(m.sessions, id)
	m.mu.Unlock()
	if ok && sess.socket != nil {
		m.hub.RemoveSocket(sess.socket)
	}
}

// HandleSocketIO is the /socket.io/ HTTP entry point.
func (m *Manager) HandleSocketIO(w http.ResponseWriter, r *http.Request) {
	setCORS(w, r)
	switch r.Method {
	case http.MethodOptions:
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		if strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			m.handleWS(w, r)
			return
		}
		m.handlePollGet(w, r)
	case http.MethodPost:
		m.handlePost(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func setCORS(w http.ResponseWriter, r *http.Request) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		origin = "*"
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Methods", "GET,POST,OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
}

func parseEIO(r *http.Request) int {
	if v, err := strconv.Atoi(r.URL.Query().Get("EIO")); err == nil && (v == 3 || v == 4) {
		return v
	}
	return 3 // socket.io v2 clients do not send EIO
}

func (m *Manager) handlePollGet(w http.ResponseWriter, r *http.Request) {
	eio := parseEIO(r)
	q := r.URL.Query()
	sid := q.Get("sid")
	w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
	if sid == "" {
		// Handshake: create session (queues open packet first) then drain.
		sess := m.newSession(eio)
		payload, _ := sess.waitForPoll()
		sess.lastSeen.Store(time.Now().UnixMilli())
		fmt.Fprint(w, payload)
		return
	}
	sess := m.get(sid)
	if sess == nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	sess.pollMu.Lock()
	defer sess.pollMu.Unlock()
	payload, ok := sess.waitForPoll()
	if !ok {
		if sess.closed.Load() {
			http.Error(w, "Bad request", http.StatusBadRequest)
			return
		}
		if sess.eio == 3 {
			payload = "6:6"
		} else {
			payload = "6"
		}
	}
	sess.lastSeen.Store(time.Now().UnixMilli())
	fmt.Fprint(w, payload)
}

func (m *Manager) handlePost(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("sid")
	sess := m.get(sid)
	if sess == nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxPayloadV4+1))
	if err != nil || len(body) > maxPayloadV4 {
		http.Error(w, "Payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	sess.lastSeen.Store(time.Now().UnixMilli())
	for _, pkt := range decodePollPayload(sess.eio, string(body)) {
		sess.handleEnginePacket(pkt)
	}
	w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
	fmt.Fprint(w, "ok")
}

func (m *Manager) handleWS(w http.ResponseWriter, r *http.Request) {
	eio := parseEIO(r)
	sid := r.URL.Query().Get("sid")
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	var sess *Session
	direct := false
	if sid == "" {
		sess = m.newSession(eio)
		direct = true
	} else {
		sess = m.get(sid)
		if sess == nil {
			conn.Close()
			return
		}
	}
	sess.wsMu.Lock()
	if sess.ws != nil {
		sess.wsMu.Unlock()
		conn.Close()
		return
	}
	sess.ws = conn
	sess.wsMu.Unlock()
	sess.lastSeen.Store(time.Now().UnixMilli())

	if direct {
		// websocket-only connection: already on the final transport.
		// Flush the queue (open packet + connect events) in order.
		sess.wsMu.Lock()
		sess.upgraded = true
		sess.wsMu.Unlock()
		sess.qmu.Lock()
		queued := s_takeQueue(sess)
		sess.qmu.Unlock()
		for _, p := range queued {
			sess.writeWS(p)
		}
	}
	if !direct {
		// Expect the probe sequence: 2probe -> 3probe -> 5 (upgrade).
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		for i := 0; i < 2; i++ {
			_, data, err := conn.ReadMessage()
			if err != nil {
				sess.Close()
				return
			}
			msg := string(data)
			sess.lastSeen.Store(time.Now().UnixMilli())
			if msg == "2probe" {
				sess.writeWS("3probe")
				continue
			}
			if msg == "5" {
				break
			}
			sess.Close()
			return
		}
		// Upgrade complete: flush queued packets over ws.
		sess.wsMu.Lock()
		sess.upgraded = true
		sess.wsMu.Unlock()
		sess.qmu.Lock()
		queued := s_takeQueue(sess)
		sess.qmu.Unlock()
		for _, p := range queued {
			sess.writeWS(p)
		}
		sess.cond.Broadcast()
	}
	conn.SetReadDeadline(time.Now().Add(pingIntervalV3 + pingTimeoutV3))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			sess.Close()
			return
		}
		sess.lastSeen.Store(time.Now().UnixMilli())
		sess.handleEnginePacket(string(data))
	}
}

func s_takeQueue(s *Session) []string {
	q := s.queue
	s.queue = nil
	return q
}
