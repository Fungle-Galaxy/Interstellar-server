package main

import (
	"strings"
	"sync"
	"time"
)

// ---- wire-format types (JSON field names match the TS client contract) ----

type ClientInfo struct {
	PlayerID int `json:"playerId"`
	ClientID int `json:"clientId"`
}

type LobbyInfo struct {
	Code           string `json:"code"`
	HostID         int    `json:"hostId"`
	PublicLobbyID  int    `json:"publicLobbyId"`
	ConnectedCount int    `json:"connectedCount"`
}

type PublicLobby struct {
	ID             int    `json:"id"`
	Title          string `json:"title"`
	Host           string `json:"host"`
	CurrentPlayers int    `json:"current_players"`
	MaxPlayers     int    `json:"max_players"`
	Language       string `json:"language"`
	Mods           string `json:"mods"`
	IsPublic       bool   `json:"isPublic"`
	Server         string `json:"server"`
	GameState      int    `json:"gameState"`
	StateTime      int64  `json:"stateTime"`
}

// GameState values (mirrors src/interfaces/gameState.ts).
const (
	GameStateLobby      = 0
	GameStateTasks      = 1
	GameStateDiscussion = 2
	GameStateMenu       = 3
	GameStateUnknown    = 4
)

type Signal struct {
	To   string `json:"to"`
	Data any    `json:"data"`
}

type socketState struct {
	code string // current lobby code, "" when in none
}

// Server is the application state (mirror of index.ts globals).
type Server struct {
	mu             sync.Mutex
	clients        map[string]*ClientInfo
	publicLobbies  map[string]*PublicLobby
	lobbyCodes     map[int]string
	allLobbies     map[string]*LobbyInfo
	lobbyCount     int
	connectionCount int

	peerConfig PeerConfig
	hostname   string
	hub        *Hub
}

func NewServer(cfg PeerConfig, hostname string) *Server {
	return &Server{
		clients:       make(map[string]*ClientInfo),
		publicLobbies: make(map[string]*PublicLobby),
		lobbyCodes:    make(map[int]string),
		allLobbies:    make(map[string]*LobbyInfo),
		peerConfig:    cfg,
		hostname:      hostname,
	}
}

func nowMillis() int64 { return time.Now().UnixMilli() }

func logf(level, format string, a ...any) { logger(level, format, a...) }

// truncateRunes cuts to n runes (TS substring() cuts UTF-16 units; close enough).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func (sv *Server) removePublicLobby(c string) {
	sv.mu.Lock()
	pl, ok := sv.publicLobbies[c]
	pid := -1
	if ok {
		pid = pl.ID
		delete(sv.publicLobbies, c)
		delete(sv.lobbyCodes, pid)
	}
	sv.mu.Unlock()
	if ok && pid >= 0 {
		sv.hub.EmitRoom("lobbybrowser", "remove_lobby", nil, pid)
	}
}

// leaveRoom mirrors the TS leaveroom().
func (sv *Server) leaveRoom(s *Socket, code string) {
	if code == "" {
		return
	}
	if len(code) == 6 || len(code) == 4 {
		sv.hub.Leave(s, code)
	}
	if sv.hub.RoomLen(code) <= 0 {
		sv.mu.Lock()
		delete(sv.allLobbies, code)
		sv.mu.Unlock()
		sv.removePublicLobby(code)
	}
}

func (sv *Server) registerHandlers() {
	h := sv.hub
	h.On("connect", sv.onConnect)
	h.On("disconnect", sv.onDisconnect)
	h.On("join", sv.onJoin)
	h.On("setHost", sv.onSetHost)
	h.On("id", sv.onID)
	h.On("leave", sv.onLeave)
	h.On("VAD", sv.onVAD)
	h.On("join_lobby", sv.onJoinLobby)
	h.On("lobby", sv.onLobby)
	h.On("remove_lobby", sv.onRemoveLobby)
	h.On("signal", sv.onSignal)
	h.On("lobbybrowser", sv.onLobbyBrowser)
}

func (sv *Server) onConnect(s *Socket, _ []any, _ AckFn) {
	s.State = &socketState{}
	sv.mu.Lock()
	sv.connectionCount++
	count := sv.connectionCount
	lobbies := len(sv.allLobbies)
	iceServers := make([]ICEServer, len(sv.peerConfig.ICEServers))
	copy(iceServers, sv.peerConfig.ICEServers)
	sv.mu.Unlock()
	logf("INFO", "Total connected: %d in %d lobbies", count, lobbies)

	if sv.peerConfig.IntegratedRelay.Enabled {
		iceServers = append(iceServers, ICEServer{
			URLs:       "turn:" + sv.hostname + ":" + atoi32(sv.peerConfig.IntegratedRelay.ListeningPort),
			Username:   sv.peerConfig.IntegratedRelay.DefaultUsername,
			Credential: sv.peerConfig.IntegratedRelay.DefaultPassword,
			Region:     "integrated",
		})
	}
	s.Emit("clientPeerConfig", map[string]any{
		"forceRelayOnly": sv.peerConfig.ForceRelayOnly,
		"iceServers":     iceServers,
	})
}

func (sv *Server) onDisconnect(s *Socket, _ []any, _ AckFn) {
	st := s.State.(*socketState)
	sv.leaveRoom(s, st.code)
	st.code = ""
	sv.mu.Lock()
	delete(sv.clients, s.ID)
	sv.connectionCount--
	count := sv.connectionCount
	lobbies := len(sv.allLobbies)
	sv.mu.Unlock()
	logf("INFO", "Total connected: %d in %d lobbies", count, lobbies)
}

func (sv *Server) onJoin(s *Socket, args []any, _ AckFn) {
	st := s.State.(*socketState)
	c, ok1 := argString(args, 0)
	id, ok2 := argNumber(args, 1)
	clientID, ok3 := argNumber(args, 2)
	isHost := false
	if len(args) > 3 {
		isHost, _ = argBool(args, 3)
	}
	if !ok1 || !ok2 || !ok3 {
		logf("ERROR", "Socket %s sent invalid join command", s.ID)
		s.sess.Close()
		return
	}
	staleCode := st.code

	// Snapshot of other clients already in the target room + lobby bookkeeping.
	sv.mu.Lock()
	otherClients := map[string]*ClientInfo{}
	for _, sid := range sv.hub.RoomMembers(c) {
		if sid == s.ID {
			continue
		}
		if cl, ok := sv.clients[sid]; ok {
			otherClients[sid] = cl
		}
	}
	lobby, exists := sv.allLobbies[c]
	hostToSend := 0
	if !exists {
		hostID := -1
		if isHost {
			hostID = clientID
		}
		sv.allLobbies[c] = &LobbyInfo{Code: c, HostID: hostID, PublicLobbyID: -1, ConnectedCount: 1}
	} else {
		lobby.ConnectedCount++
		if isHost {
			lobby.HostID = clientID
		}
		hostToSend = lobby.HostID
	}
	sv.mu.Unlock()

	// TS: socket.to(oldCode).broadcast.emit('setHost', clientId)
	if exists && isHost && staleCode != "" {
		sv.hub.EmitRoom(staleCode, "setHost", s, clientID)
	}
	if exists {
		s.Emit("setHost", hostToSend)
	}

	if staleCode != c {
		sv.leaveRoom(s, staleCode)
	}
	st.code = c
	sv.hub.Join(s, c)
	sv.hub.EmitRoom(c, "join", s, s.ID, map[string]int{"playerId": id, "clientId": clientID})
	s.Emit("setClients", otherClients)
}

func (sv *Server) onSetHost(s *Socket, args []any, _ AckFn) {
	st := s.State.(*socketState)
	c, ok1 := argString(args, 0)
	clientID, ok2 := argNumber(args, 1)
	if !ok1 || !ok2 || st.code != c {
		return
	}
	sv.mu.Lock()
	lobby, ok := sv.allLobbies[c]
	if ok {
		lobby.HostID = clientID
	}
	sv.mu.Unlock()
	if ok {
		sv.hub.EmitRoom(c, "setHost", s, clientID)
	}
}

func (sv *Server) onID(s *Socket, args []any, _ AckFn) {
	st := s.State.(*socketState)
	id, ok1 := argNumber(args, 0)
	clientID, ok2 := argNumber(args, 1)
	if !ok1 || !ok2 {
		logf("ERROR", "Socket %s sent invalid id command", s.ID)
		s.sess.Close()
		return
	}
	sv.mu.Lock()
	existing := sv.clients[s.ID]
	if existing != nil && existing.ClientID != clientID {
		logf("ERROR", "Socket %s->%d->%d->%d sent invalid id command, attempted spoofing another client",
			s.ID, existing.ClientID, clientID, id)
	}
	client := &ClientInfo{PlayerID: id, ClientID: clientID}
	sv.clients[s.ID] = client
	sv.mu.Unlock()
	if st.code != "" {
		sv.hub.EmitRoom(st.code, "setClient", s, s.ID, client)
	}
}

func (sv *Server) onLeave(s *Socket, _ []any, _ AckFn) {
	st := s.State.(*socketState)
	if st.code != "" {
		sv.leaveRoom(s, st.code)
		st.code = ""
		sv.mu.Lock()
		delete(sv.clients, s.ID)
		sv.mu.Unlock()
	}
}

func (sv *Server) onVAD(s *Socket, args []any, _ AckFn) {
	st := s.State.(*socketState)
	activity, ok := argBool(args, 0)
	if !ok || st.code == "" {
		return
	}
	sv.mu.Lock()
	client := sv.clients[s.ID]
	sv.mu.Unlock()
	if client == nil {
		return
	}
	sv.hub.EmitRoom(st.code, "VAD", s, map[string]any{
		"activity": activity,
		"client":   client,
		"socketId": s.ID,
	})
}

func (sv *Server) onJoinLobby(s *Socket, args []any, ack AckFn) {
	id, ok := argNumber(args, 0)
	if !ok || ack == nil {
		return
	}
	sv.mu.Lock()
	code, hasCode := sv.lobbyCodes[id]
	pl, hasLobby := sv.publicLobbies[code]
	var snapshot PublicLobby
	if hasLobby {
		snapshot = *pl
	}
	sv.mu.Unlock()
	if hasCode && hasLobby {
		if snapshot.IsPublic && snapshot.GameState == GameStateLobby {
			ack(0, code, snapshot.Server, snapshot)
			return
		}
		ack(1, "Lobby is not public anymore")
		return
	}
	ack(1, "Lobby not found :C")
}

// field helpers for the loosely-typed "lobby" argument.

func strField(m map[string]any, key string, maxRunes int, def string) string {
	v, exists := m[key]
	if !exists || v == nil {
		return def
	}
	s, ok := v.(string)
	if !ok {
		return def
	}
	if maxRunes > 0 {
		return truncateRunes(s, maxRunes)
	}
	return s
}

func numField(m map[string]any, key string) int {
	v, exists := m[key]
	if !exists {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int(n)
	}
	return 0
}

func boolField(m map[string]any, key string) bool {
	v, exists := m[key]
	if !exists {
		return false
	}
	b, _ := v.(bool)
	return b
}

func (sv *Server) onLobby(s *Socket, args []any, _ AckFn) {
	st := s.State.(*socketState)
	c, ok1 := argString(args, 0)
	pl, ok2 := argObject(args, 1)
	if !ok1 || !ok2 {
		return
	}
	if st.code != c {
		logf("ERROR", "Got request to host lobby while not in it %s", c)
		return
	}
	isPublic := boolField(pl, "isPublic")
	isPublic2 := boolField(pl, "isPublic2")
	if !isPublic && !isPublic2 {
		sv.removePublicLobby(c)
		return
	}
	gameState := numField(pl, "gameState")

	sv.mu.Lock()
	old, hasOld := sv.publicLobbies[c]
	id := sv.lobbyCount
	if hasOld {
		id = old.ID
	} else {
		sv.lobbyCount++
	}
	var stateTime int64
	if hasOld && ((old.GameState == GameStateLobby && gameState == GameStateLobby) ||
		(old.GameState != GameStateLobby && gameState != GameStateLobby)) {
		stateTime = old.StateTime
	} else {
		stateTime = nowMillis()
	}
	lobby := &PublicLobby{
		ID:             id,
		Title:          strField(pl, "title", 20, "ERROR"),
		Host:           strField(pl, "host", 10, ""),
		CurrentPlayers: numField(pl, "current_players"),
		MaxPlayers:     numField(pl, "max_players"),
		Language:       strField(pl, "language", 5, ""),
		Mods:           strings.ToUpper(strField(pl, "mods", 20, "")),
		IsPublic:       true,
		Server:         strField(pl, "server", 0, ""),
		GameState:      gameState,
		StateTime:      stateTime,
	}
	sv.lobbyCodes[id] = c
	sv.publicLobbies[c] = lobby
	snapshot := *lobby
	sv.mu.Unlock()
	sv.hub.EmitRoom("lobbybrowser", "update_lobby", nil, snapshot)
}

func (sv *Server) onRemoveLobby(s *Socket, args []any, _ AckFn) {
	st := s.State.(*socketState)
	c, ok := argString(args, 0)
	if !ok {
		return
	}
	if st.code != c {
		logf("ERROR", "Got request to host lobby while not in it %s", c)
		return
	}
	sv.removePublicLobby(c)
}

func (sv *Server) onSignal(s *Socket, args []any, _ AckFn) {
	sig, ok := argObject(args, 0)
	if !ok {
		logf("ERROR", "Socket %s sent invalid signal command", s.ID)
		s.sess.Close()
		return
	}
	to, okTo := sig["to"].(string)
	data, hasData := sig["data"]
	if !hasData || data == nil || !okTo || to == "" {
		logf("ERROR", "Socket %s sent invalid signal command", s.ID)
		s.sess.Close()
		return
	}
	sv.hub.EmitTo(to, "signal", map[string]any{
		"data": data,
		"from": s.ID,
	})
}

func (sv *Server) onLobbyBrowser(s *Socket, args []any, _ AckFn) {
	open, ok := argBool(args, 0)
	if !ok {
		return
	}
	if !open {
		sv.hub.Leave(s, "lobbybrowser")
		return
	}
	sv.hub.Join(s, "lobbybrowser")
	sv.mu.Lock()
	lobbies := make([]PublicLobby, 0, len(sv.publicLobbies))
	for _, pl := range sv.publicLobbies {
		lobbies = append(lobbies, *pl)
	}
	sv.mu.Unlock()
	s.Emit("new_lobbies", lobbies)
}
