package main

import (
	"encoding/json"
	"log"
	"strconv"
	"sync"
)

// AckFn replies to a client-side acknowledgement callback.
type AckFn func(args ...any)

// EventHandler handles a socket.io event. ack may be nil.
type EventHandler func(s *Socket, args []any, ack AckFn)

// Socket is one connected client (one Engine.IO session).
type Socket struct {
	ID   string
	sess *Session
	hub  *Hub

	mu    sync.Mutex
	rooms map[string]bool
	State any // per-socket application state (set by handler.go)
}

// Hub tracks sockets/rooms and dispatches socket.io packets.
type Hub struct {
	mu       sync.Mutex
	sockets  map[string]*Socket
	rooms    map[string]map[string]bool // room name -> set of socket ids
	handlers map[string]EventHandler
}

func NewHub() *Hub {
	return &Hub{
		sockets:  make(map[string]*Socket),
		rooms:    make(map[string]map[string]bool),
		handlers: make(map[string]EventHandler),
	}
}

func (h *Hub) On(event string, fn EventHandler) {
	h.handlers[event] = fn
}

// CreateSocket registers a socket for an engine.io session (no events yet).
func (h *Hub) CreateSocket(sess *Session) *Socket {
	s := &Socket{
		ID:    sess.id,
		sess:  sess,
		hub:   h,
		rooms: make(map[string]bool),
	}
	h.mu.Lock()
	h.sockets[s.ID] = s
	h.mu.Unlock()
	return s
}

// FireConnect runs the connect handler (after the open packet is queued).
func (h *Hub) FireConnect(s *Socket) {
	if fn, ok := h.handlers["connect"]; ok {
		fn(s, nil, nil)
	}
}

// RemoveSocket cleans rooms and fires the disconnect handler.
func (h *Hub) RemoveSocket(s *Socket) {
	h.mu.Lock()
	delete(h.sockets, s.ID)
	for room := range s.rooms {
		if members, ok := h.rooms[room]; ok {
			delete(members, s.ID)
			if len(members) == 0 {
				delete(h.rooms, room)
			}
		}
	}
	h.mu.Unlock()
	if fn, ok := h.handlers["disconnect"]; ok {
		fn(s, nil, nil)
	}
}

func (h *Hub) Join(s *Socket, room string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	members, ok := h.rooms[room]
	if !ok {
		members = make(map[string]bool)
		h.rooms[room] = members
	}
	members[s.ID] = true
	s.rooms[room] = true
}

func (h *Hub) Leave(s *Socket, room string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if members, ok := h.rooms[room]; ok {
		delete(members, s.ID)
		if len(members) == 0 {
			delete(h.rooms, room)
		}
	}
	delete(s.rooms, room)
}

// RoomLen returns the number of sockets in a room.
func (h *Hub) RoomLen(room string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.rooms[room])
}

// RoomMembers returns a snapshot of socket ids in a room.
func (h *Hub) RoomMembers(room string) []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.rooms[room]))
	for id := range h.rooms[room] {
		out = append(out, id)
	}
	return out
}

func (h *Hub) get(id string) *Socket {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sockets[id]
}

// Emit sends an event to one socket.
func (s *Socket) Emit(event string, args ...any) {
	s.emit(event, args)
}

func (s *Socket) emit(event string, args []any) {
	payload := make([]any, 0, len(args)+1)
	payload = append(payload, event)
	payload = append(payload, args...)
	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("ERROR: failed to marshal event %s: %v", event, err)
		return
	}
	s.sess.send("42" + string(data))
}

// EmitRoom sends an event to every socket in a room; except==nil includes all.
func (h *Hub) EmitRoom(room, event string, except *Socket, args ...any) {
	h.mu.Lock()
	members := make([]*Socket, 0, len(h.rooms[room]))
	for id := range h.rooms[room] {
		if except != nil && id == except.ID {
			continue
		}
		if s, ok := h.sockets[id]; ok {
			members = append(members, s)
		}
	}
	h.mu.Unlock()
	for _, s := range members {
		s.emit(event, args)
	}
}

// EmitTo emits to a socket id (socket.io treats sids as implicit rooms).
func (h *Hub) EmitTo(id, event string, args ...any) {
	if s := h.get(id); s != nil {
		s.emit(event, args)
	}
}

// Dispatch parses and handles one socket.io packet payload (after the '4').
func (h *Hub) Dispatch(s *Socket, payload string) {
	if payload == "" {
		return
	}
	typ := payload[0]
	rest := payload[1:]
	// Optional acknowledgement id.
	ackID := ""
	for len(rest) > 0 && rest[0] >= '0' && rest[0] <= '9' {
		ackID += string(rest[0])
		rest = rest[1:]
	}
	// Optional namespace (e.g. "/admin,") — we only serve "/".
	if len(rest) > 0 && rest[0] == '/' {
		comma := -1
		for i := 0; i < len(rest); i++ {
			if rest[i] == ',' {
				comma = i
				break
			}
		}
		if comma < 0 {
			rest = ""
		} else {
			rest = rest[comma+1:]
		}
	}
	switch typ {
	case '0': // CONNECT
		if s.sess.eio == 4 {
			s.sess.send(`40{"sid":"` + s.ID + `"}`)
		} else {
			s.sess.send("40")
		}
	case '1': // DISCONNECT
		s.sess.Close()
	case '2': // EVENT
		var fields []json.RawMessage
		if err := json.Unmarshal([]byte(rest), &fields); err != nil || len(fields) == 0 {
			log.Printf("ERROR: socket %s sent malformed event: %s", s.ID, rest)
			return
		}
		var event string
		if err := json.Unmarshal(fields[0], &event); err != nil {
			log.Printf("ERROR: socket %s sent event with bad name: %s", s.ID, rest)
			return
		}
		args := make([]any, 0, len(fields)-1)
		for _, raw := range fields[1:] {
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				log.Printf("ERROR: socket %s sent unparseable arg in %s: %v", s.ID, event, err)
				return
			}
			args = append(args, v)
		}
		fn, ok := h.handlers[event]
		if !ok {
			return // unknown event: ignore, like socket.io
		}
		var ack AckFn
		if ackID != "" {
			id := ackID
			ack = func(a ...any) {
				data, err := json.Marshal(a)
				if err != nil {
					log.Printf("ERROR: failed to marshal ack for %s: %v", event, err)
					return
				}
				s.sess.send("43" + id + string(data))
			}
		}
		fn(s, args, ack)
	case '4': // CONNECT_ERROR from client: ignore
	case '5': // BINARY_EVENT: not used by BCL
	case '6': // BINARY_ACK: not used by BCL
	}
}

// --- typed argument extraction helpers (mirror the TS typeof checks) ---

func argString(args []any, i int) (string, bool) {
	if i >= len(args) {
		return "", false
	}
	v, ok := args[i].(string)
	return v, ok
}

func argNumber(args []any, i int) (int, bool) {
	if i >= len(args) {
		return 0, false
	}
	switch v := args[i].(type) {
	case float64:
		if v != float64(int(v)) {
			return 0, false
		}
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	}
	return 0, false
}

func argBool(args []any, i int) (bool, bool) {
	if i >= len(args) {
		return false, false
	}
	v, ok := args[i].(bool)
	return v, ok
}

func argObject(args []any, i int) (map[string]any, bool) {
	if i >= len(args) {
		return nil, false
	}
	v, ok := args[i].(map[string]any)
	return v, ok
}

func atoi32(n int) string { return strconv.Itoa(n) }
