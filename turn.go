package main

import (
	"crypto/md5"
	"fmt"
	"net"

	"github.com/pion/logging"
	"github.com/pion/turn/v2"
	"github.com/pion/transport/v2/stdnet"
)

// detectExternalIP guesses the public IP by opening a UDP "connection" to a
// well-known address (no traffic is actually sent).
func detectExternalIP() (net.IP, error) {
	conn, err := net.Dial("udp4", "8.8.8.8:53")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	host, _, err := net.SplitHostPort(conn.LocalAddr().String())
	if err != nil {
		return nil, err
	}
	ip := net.ParseIP(host)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() {
		return nil, fmt.Errorf("detected local IP %s is not a public address", host)
	}
	return ip, nil
}

func mapDebugLevel(level string) logging.LogLevel {
	switch level {
	case "OFF":
		return logging.LogLevelDisabled
	case "FATAL", "ERROR":
		return logging.LogLevelError
	case "WARN":
		return logging.LogLevelWarn
	case "DEBUG":
		return logging.LogLevelDebug
	case "TRACE":
		return logging.LogLevelTrace
	default:
		return logging.LogLevelInfo
	}
}

// StartIntegratedTurn boots the embedded TURN server (parity with node-turn).
func StartIntegratedTurn(cfg IntegratedRelaySettings) (*turn.Server, error) {
	listenIP := "0.0.0.0"
	if len(cfg.ListeningIps) > 0 {
		listenIP = cfg.ListeningIps[0]
	}
	pc, err := net.ListenPacket("udp4", fmt.Sprintf("%s:%d", listenIP, cfg.ListeningPort))
	if err != nil {
		return nil, fmt.Errorf("integratedRelay: failed to listen on %s:%d: %v", listenIP, cfg.ListeningPort, err)
	}

	var extIP net.IP
	if len(cfg.ExternalIps) > 0 && cfg.ExternalIps[0] != "" {
		extIP = net.ParseIP(cfg.ExternalIps[0])
		if extIP == nil {
			pc.Close()
			return nil, fmt.Errorf("integratedRelay: externalIps[0] %q is not a valid IP", cfg.ExternalIps[0])
		}
	} else {
		extIP, err = detectExternalIP()
		if err != nil {
			pc.Close()
			return nil, fmt.Errorf("integratedRelay: could not auto-detect external IP (%v); "+
				"set integratedRelay.externalIps in peerConfig.yml explicitly", err)
		}
	}
	logf("INFO", "integratedRelay: external IP set to %s (relay range %d-%d)",
		extIP, cfg.MinPort, cfg.MaxPort)

	n, err := stdnet.NewNet()
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("integratedRelay: stdnet: %v", err)
	}

	lf := logging.NewDefaultLoggerFactory()
	lf.DefaultLogLevel = mapDebugLevel(cfg.DebugLevel)

	srv, err := turn.NewServer(turn.ServerConfig{
		Realm: "crewlink",
		AuthHandler: func(username, realm string, srcAddr net.Addr) ([]byte, bool) {
			if username == cfg.DefaultUsername {
				// Long-term credential mechanism: the key is MD5(user:realm:pass).
				h := md5.New() //nolint:gosec // required by RFC 5389
				fmt.Fprintf(h, "%s:%s:%s", username, realm, cfg.DefaultPassword)
				return h.Sum(nil), true
			}
			return nil, false
		},
		PacketConnConfigs: []turn.PacketConnConfig{
			{
				PacketConn: pc,
				RelayAddressGenerator: &turn.RelayAddressGeneratorPortRange{
					RelayAddress: extIP,
					MinPort:      uint16(cfg.MinPort),
					MaxPort:      uint16(cfg.MaxPort),
					MaxRetries:   60,
					Address:      "0.0.0.0",
					Net:          n,
				},
			},
		},
		LoggerFactory: lf,
	})
	if err != nil {
		pc.Close()
		return nil, fmt.Errorf("integratedRelay: failed to start TURN server: %v", err)
	}
	logf("INFO", "integratedRelay: TURN server listening on %s:%d (udp)", listenIP, cfg.ListeningPort)
	return srv, nil
}
