package main

import (
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var startTime = time.Now()

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

//go:embed Logo.png
var logoPNG []byte

// logger prints in the same style as the TS server's tracer output:
// 2026-08-16T10:00:00Z <INFO> message
func logger(level, format string, a ...any) {
	log.Printf("<%s> %s", level, fmt.Sprintf(format, a...))
}

var peerCfg PeerConfig

type healthResponse struct {
	Uptime          float64 `json:"uptime"`
	ConnectionCount int     `json:"connectionCount"`
	LobbiesCount    int     `json:"lobbiesCount"`
	Address         string  `json:"address"`
	Name            string  `json:"name,omitempty"`
	Version         string  `json:"version"`
}

// pageText holds every user-facing string of the status page for one language.
type pageText struct {
	Lang     string // value for <html lang="...">
	Sub      string
	Online   string
	Rooms    string
	Uptime   string
	Addr     string
	Download string
	Switch   string // language switcher link text
	SwitchTo string // language switcher href
	UD       string // uptime units consumed by the JS formatter
	UH       string
	UM       string
	US       string
}

var pageTexts = map[string]pageText{
	"en": {
		Lang: "en", Sub: "Interstellar proximity voice · Go signaling server",
		Online: "Online", Rooms: "Rooms", Uptime: "Uptime",
		Addr: "Server address", Download: "Download Interstellar Client",
		Switch: "中文", SwitchTo: "?lang=zh",
		UD: "d ", UH: "h ", UM: "m ", US: "s",
	},
	"zh": {
		Lang: "zh", Sub: "Interstellar 临近语音 · Go 版信令服务器",
		Online: "在线", Rooms: "房间", Uptime: "运行",
		Addr: "服务器地址", Download: "下载 Interstellar 客户端",
		Switch: "English", SwitchTo: "?lang=en",
		UD: "天", UH: "时", UM: "分", US: "秒",
	},
}

// pickLang resolves the page language: ?lang= > cookie > Accept-Language > en.
func pickLang(r *http.Request) string {
	if q := r.URL.Query().Get("lang"); q == "en" || q == "zh" {
		return q
	}
	if c, err := r.Cookie("lang"); err == nil && (c.Value == "en" || c.Value == "zh") {
		return c.Value
	}
	// Highest-q entry among zh/en wins; on ties the earlier entry wins.
	best, bestQ := "", -1.0
	for _, part := range strings.Split(r.Header.Get("Accept-Language"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		media, q := part, 1.0
		if i := strings.Index(part, ";"); i >= 0 {
			media = strings.TrimSpace(part[:i])
			for _, p := range strings.Split(part[i+1:], ";") {
				if p = strings.TrimSpace(p); strings.HasPrefix(p, "q=") {
					if v, err := strconv.ParseFloat(strings.TrimPrefix(p, "q="), 64); err == nil {
						q = v
					}
				}
			}
		}
		base := strings.Split(strings.ToLower(media), "-")[0]
		if (base == "zh" || base == "en") && q > bestQ {
			best, bestQ = base, q
		}
	}
	if best == "" {
		return "en"
	}
	return best
}

type pageData struct {
	Address     string
	Connections int
	Lobbies     int
	pageText
}

var indexTmpl = template.Must(template.New("index").Parse(`<!DOCTYPE html>
<html lang="{{.Lang}}">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Interstellar Server</title>
<link rel="icon" type="image/png" href="/logo.png">
<style>
  :root { --bg:#0d0d1a; --accent:#6ea8ff; --text:#e0e0e0; --muted:#999; }
  *, *::before, *::after { box-sizing:border-box; margin:0; padding:0; }

  body {
    min-height:100vh; display:flex; align-items:center; justify-content:center;
    font-family:'Segoe UI', system-ui, -apple-system, Roboto, sans-serif;
    color:var(--text); background:var(--bg);
  }

  /* Animated starfield background */
  #stars {
    position:fixed; inset:0; width:100%; height:100%;
    background:
      radial-gradient(ellipse at 20% 50%, rgba(120,119,198,.15), transparent 50%),
      radial-gradient(ellipse at 80% 20%, rgba(65,105,225,.1), transparent 40%),
      #0d0d1a;
    z-index:0;
  }

  .container {
    position:relative; z-index:1; text-align:center;
    width:min(460px,92vw); margin:24px;
  }

  /* Hero logo floating on the starfield */
  .logo {
    display:block; width:70%; max-width:480px;
    margin:0 auto 1.5rem;
    filter:drop-shadow(0 4px 24px rgba(100,120,255,.35));
  }

  h1 { font-size:1.6rem; letter-spacing:-.5px; font-weight:700; margin-bottom:6px; }
  h1 span { color:var(--accent); }
  .sub { color:var(--muted); font-size:13px; margin-bottom:20px; }

  .card {
    background:rgba(255,255,255,.05); border:1px solid rgba(255,255,255,.1);
    border-radius:12px; padding:1.5rem 2rem; backdrop-filter:blur(10px);
    box-shadow:0 24px 70px rgba(0,0,0,.55);
  }
  .stats { display:grid; grid-template-columns:repeat(3,1fr); gap:10px; margin-bottom:18px; }
  .stat { background:rgba(13,13,26,.55); border:1px solid rgba(255,255,255,.08); border-radius:10px; padding:12px 6px; text-align:center; }
  .stat b { display:block; font-size:20px; color:#7eb3ff; font-variant-numeric:tabular-nums; }
  .stat i { font-style:normal; font-size:11px; color:var(--muted); letter-spacing:1px; }
  .addr { font-size:13px; color:var(--muted); word-break:break-all; margin-bottom:18px; }
  .addr b { color:var(--text); font-weight:600; }
  .btn {
    display:inline-block; padding:.6rem 1.25rem; border-radius:8px; background:#3b6fd4;
    color:#fff; border:1px solid #3b6fd4; font-weight:600; font-size:14px;
    text-decoration:none; transition:background .15s, border-color .15s, transform .1s;
  }
  .btn:hover { background:#4d80e8; border-color:#4d80e8; }
  .btn:active { transform:scale(.97); }
  footer { margin-top:16px; font-size:11px; color:var(--muted); text-align:center; }
  .lang {
    display:inline-block; margin-top:10px; padding:3px 14px;
    font-size:11px; color:var(--muted); text-decoration:none;
    border:1px solid rgba(255,255,255,.18); border-radius:999px;
    transition:color .15s, border-color .15s, background .15s;
  }
  .lang:hover { color:#fff; border-color:rgba(255,255,255,.45); background:rgba(255,255,255,.06); }
</style>
</head>
<body>
<canvas id="stars"></canvas>
<div class="container">
  <img class="logo" src="/logo.png" alt="Interstellar Server Logo">
  <h1>Inter<span>stellar</span> Server</h1>
  <p class="sub">{{.Sub}}</p>
  <div class="card">
    <div class="stats">
      <div class="stat"><b id="conn">{{.Connections}}</b><i>{{.Online}}</i></div>
      <div class="stat"><b id="lobbies">{{.Lobbies}}</b><i>{{.Rooms}}</i></div>
      <div class="stat"><b id="uptime">–</b><i>{{.Uptime}}</i></div>
    </div>
    <p class="addr">{{.Addr}} <b>{{.Address}}</b></p>
    <a class="btn" href="https://github.com/Fungle-Galaxy/Interstellar" target="_blank" rel="noopener">{{.Download}}</a>
  </div>
  <footer id="ver">Interstellar Server</footer>
  <a class="lang" href="{{.SwitchTo}}">{{.Switch}}</a>
</div>
<script>
  (function () {
    var canvas = document.getElementById('stars');
    var ctx = canvas.getContext('2d');
    var stars = [];
    var N = 180;

    function resize() {
      canvas.width = window.innerWidth;
      canvas.height = window.innerHeight;
    }

    for (var i = 0; i < N; i++) {
      stars.push({
        x: Math.random(), y: Math.random(),
        r: Math.random() * 1.3 + 0.2,
        speed: Math.random() * 0.00008 + 0.00002,
        base: Math.random() * 0.5 + 0.3,
        phase: Math.random() * Math.PI * 2
      });
    }

    var t = 0;
    function draw() {
      t += 0.02;
      ctx.clearRect(0, 0, canvas.width, canvas.height);
      for (var j = 0; j < stars.length; j++) {
        var s = stars[j];
        var opacity = s.base + 0.25 * Math.sin(t + s.phase);
        ctx.beginPath();
        ctx.arc(s.x * canvas.width, s.y * canvas.height, s.r, 0, Math.PI * 2);
        ctx.fillStyle = 'rgba(255,255,255,' + opacity + ')';
        ctx.fill();
        s.y += s.speed;
        if (s.y > 1) { s.y = 0; s.x = Math.random(); }
      }
      requestAnimationFrame(draw);
    }

    resize();
    window.addEventListener('resize', resize);
    draw();
  })();
</script>
<script>
  const fmt = (s) => {
    s = Math.floor(s);
    const d = Math.floor(s/86400), h = Math.floor(s%86400/3600), m = Math.floor(s%3600/60);
    return (d ? d + '{{.UD}}' + h + '{{.UH}}'
      : h ? h + '{{.UH}}' + m + '{{.UM}}'
      : m ? m + '{{.UM}}'
      : s + '{{.US}}').trim();
  };
  async function tick() {
    try {
      const h = await (await fetch('/health')).json();
      conn.textContent = h.connectionCount;
      lobbies.textContent = h.lobbiesCount;
      uptime.textContent = fmt(h.uptime);
      if (h.version) ver.textContent = 'Interstellar Server ' + h.version;
    } catch (e) {}
  }
  tick();
  setInterval(tick, 5000);
</script>
</body>
</html>
`))

func requestAddress(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	return scheme + "://" + r.Host
}

func main() {
	configPath := flag.String("config", "", "path to peerConfig.yml (default: ./config/peerConfig.yml or $PEER_CONFIG)")
	flag.Parse()

	httpsEnabled := os.Getenv("HTTPS") != ""
	port := os.Getenv("PORT")
	if port == "" {
		if httpsEnabled {
			port = "443"
		} else {
			port = "9736"
		}
	}
	sslPath := os.Getenv("SSLPATH")
	if sslPath == "" {
		sslPath = os.Getenv("PWD")
	}
	hostname := os.Getenv("HOSTNAME")
	name := os.Getenv("NAME")

	path := *configPath
	if path == "" {
		path = os.Getenv("PEER_CONFIG")
	}
	if path == "" {
		path = filepath.Join("config", "peerConfig.yml")
	}
	cfg, err := LoadPeerConfig(path)
	if err != nil {
		logger("FATAL", "Unable to load peer config: %v", err)
		os.Exit(1)
	}
	peerCfg = cfg

	if cfg.IntegratedRelay.Enabled && hostname == "" {
		logger("FATAL", "You must set the HOSTNAME environment variable to use the integrated TURN server.")
		os.Exit(1)
	}

	var turnSrv interface{ Close() error }
	if cfg.IntegratedRelay.Enabled {
		srv, err := StartIntegratedTurn(cfg.IntegratedRelay)
		if err != nil {
			logger("FATAL", "%v", err)
			os.Exit(1)
		}
		turnSrv = srv
	}

	hub := NewHub()
	server := NewServer(cfg, hostname)
	server.hub = hub
	server.registerHandlers()
	mgr := NewManager(hub)

	mux := http.NewServeMux()
	mux.HandleFunc("/socket.io/", mgr.HandleSocketIO)
	mux.HandleFunc("/logo.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		w.Write(logoPNG)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		hub.mu.Lock()
		lobbies := len(server.allLobbies)
		connections := server.connectionCount
		hub.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(healthResponse{
			Uptime:          time.Since(startTime).Seconds(),
			ConnectionCount: connections,
			LobbiesCount:    lobbies,
			Address:         requestAddress(r),
			Name:            name,
			Version:         version,
		})
	})
	mux.HandleFunc("/lobbies", func(w http.ResponseWriter, r *http.Request) {
		server.mu.Lock()
		lobbies := make([]PublicLobby, 0, len(server.publicLobbies))
		for _, pl := range server.publicLobbies {
			lobbies = append(lobbies, *pl)
		}
		server.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(lobbies)
	})
	mux.Handle("/public/", http.StripPrefix("/public/", http.FileServer(http.Dir("public"))))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		lang := pickLang(r)
		if q := r.URL.Query().Get("lang"); q == "en" || q == "zh" {
			http.SetCookie(w, &http.Cookie{
				Name: "lang", Value: q, Path: "/",
				MaxAge: 365 * 24 * 3600, SameSite: http.SameSiteLaxMode,
			})
		}
		hub.mu.Lock()
		lobbies := len(server.allLobbies)
		connections := server.connectionCount
		hub.mu.Unlock()
		indexTmpl.Execute(w, pageData{
			Address:     requestAddress(r),
			Connections: connections,
			Lobbies:     lobbies,
			pageText:    pageTexts[lang],
		})
	})

	httpSrv := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		logger("INFO", "Shutting down...")
		if turnSrv != nil {
			turnSrv.Close()
		}
		os.Exit(0)
	}()

	logger("INFO", "Interstellar Server %s started on port %s", version, port)
	if httpsEnabled {
		cert := filepath.Join(sslPath, "fullchain.pem")
		key := filepath.Join(sslPath, "privkey.pem")
		if err := httpSrv.ListenAndServeTLS(cert, key); err != nil {
			logger("FATAL", "%v", err)
			os.Exit(1)
		}
	} else {
		if err := httpSrv.ListenAndServe(); err != nil {
			logger("FATAL", "%v", err)
			os.Exit(1)
		}
	}
}
