// FreeProxy - a free, open-source Minecraft: Bedrock proxy that runs in Termux.
// Made for hihellohejo. Built on gophertunnel (MIT).
package core

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/df-mc/go-playfab/v2"
	"github.com/df-mc/go-xsapi/v2"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/auth"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/realms"
	"github.com/sandertv/gophertunnel/minecraft/service"
	"github.com/wlynxg/anet"
	"golang.org/x/oauth2"
)

const version = "1.0"

type config struct {
	Mode       string `json:"mode"` // "realm" or "server"
	RealmID    int    `json:"realm_id,omitempty"`
	RealmName  string `json:"realm_name,omitempty"`
	Address    string `json:"address,omitempty"`
	ListenPort int    `json:"listen_port"`
}

var (
	stdin  *bufio.Reader
	out    io.Writer = os.Stdout
	logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
)

var defPort = 19132

func runMain(args []string, listenHost string, defaultPort int) {
	defPort = defaultPort
	fixAndroidDNS()
	fmt.Fprintf(out, "\n=== FreeProxy v%s (Minecraft %s) ===\n\n", version, protocol.CurrentVersion)

	if len(args) > 0 && args[0] == "reset" {
		_ = os.Remove("config.json")
		fmt.Fprintln(out, "Target cleared. Run again to pick a new Realm/server.")
		return
	}
	if len(args) > 0 && args[0] == "logout" {
		_ = os.Remove("token.json")
		_ = os.Remove("config.json")
		fmt.Fprintln(out, "Logged out.")
		return
	}

	var src oauth2.TokenSource
	var cfg config
	if t := os.Getenv("FREEPROXY_TEST_TARGET"); t != "" { // developer self-test, no login
		cfg = config{Mode: "server", Address: t, ListenPort: 19140}
	} else {
		src = loadTokenSource()
		cfg = loadConfig(src)
	}

	listener, err := minecraft.ListenConfig{
		StatusProvider:         minecraft.NewStatusProvider("FreeProxy", "FreeProxy"),
		AuthenticationDisabled: true,
		ErrorLog:               logger,
	}.Listen("raknet", fmt.Sprintf("%s:%d", listenHost, cfg.ListenPort))
	if err != nil {
		fatal("could not open port %d: %v", cfg.ListenPort, err)
	}
	defer listener.Close()

	fmt.Fprintln(out, "Proxy is running!")
	if listenHost == "127.0.0.1" {
		fmt.Fprintf(out, "  Now open Minecraft > Play > Servers > Add Server\n    Address: 127.0.0.1   Port: %d\n", cfg.ListenPort)
		fmt.Fprintln(out, "  Keep this app open in the background (don't swipe it away).")
	} else {
		for _, ip := range localIPs() {
			fmt.Fprintf(out, "  On your iPhone: Minecraft > Play > Servers > Add Server\n    Address: %s   Port: %d\n", ip, cfg.ListenPort)
		}
		fmt.Fprintln(out, "  (It may also show up under the Friends tab as a LAN game.)")
	}
	fmt.Fprint(out, "  Type .help in Minecraft chat once you're in.\n\n")

	for {
		c, err := listener.Accept()
		if err != nil {
			fatal("listener stopped: %v", err)
		}
		go handleConn(c.(*minecraft.Conn), listener, cfg, src)
	}
}

// ---------- login ----------

type savingSource struct {
	src  oauth2.TokenSource
	mu   sync.Mutex
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.RefreshToken != s.last {
		s.last = t.RefreshToken
		if b, err := json.Marshal(t); err == nil {
			_ = os.WriteFile("token.json", b, 0600)
		}
	}
	return t, nil
}

func loadTokenSource() oauth2.TokenSource {
	var tok *oauth2.Token
	if b, err := os.ReadFile("token.json"); err == nil {
		tok = new(oauth2.Token)
		if json.Unmarshal(b, tok) != nil {
			tok = nil
		}
	}
	if tok == nil {
		fmt.Fprintln(out, "Step 1: sign in to the Xbox/Microsoft account you play Minecraft with.")
		fmt.Fprint(out, "Open the link below on ANY device, enter the code, and sign in:\n\n")
		t, err := auth.RequestLiveTokenWriter(out)
		if err != nil {
			fatal("login failed: %v", err)
		}
		tok = t
		fmt.Fprint(out, "\nSigned in! (saved, you won't need to do this again)\n\n")
	}
	s := &savingSource{src: auth.RefreshTokenSource(tok)}
	if _, err := s.Token(); err != nil {
		_ = os.Remove("token.json")
		fatal("saved login expired (%v). Run again to sign in.", err)
	}
	return s
}

// ---------- target selection ----------

func ask(q string) string {
	fmt.Fprint(out, q)
	s, _ := stdin.ReadString('\n')
	return strings.TrimSpace(s)
}

func loadConfig(src oauth2.TokenSource) config {
	var cfg config
	if b, err := os.ReadFile("config.json"); err == nil && json.Unmarshal(b, &cfg) == nil && cfg.Mode != "" {
		if cfg.ListenPort == 0 {
			cfg.ListenPort = defPort
		}
		if cfg.Mode == "realm" {
			fmt.Fprintf(out, "Target: Realm \"%s\"  (run `./freeproxy reset` to change)\n\n", cfg.RealmName)
		} else {
			fmt.Fprintf(out, "Target: server %s  (run `./freeproxy reset` to change)\n\n", cfg.Address)
		}
		return cfg
	}
	cfg.ListenPort = defPort
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	fmt.Fprintln(out, "Step 2: where do you want to connect?")
	rc := realms.NewClient(src, nil)
	list, err := rc.Realms(ctx)
	if err != nil {
		fmt.Fprintln(out, "  (couldn't load your Realms:", err, ")")
	}
	for i, r := range list {
		state := r.State
		if r.Expired {
			state = "EXPIRED"
		}
		fmt.Fprintf(out, "  %d) Realm: %s  [owner: %s, %s]\n", i+1, r.Name, r.Owner, state)
	}
	fmt.Fprintln(out, "  c) Join a Realm with an invite code/link")
	fmt.Fprintln(out, "  s) A normal server (IP address)")
	for {
		ch := strings.ToLower(ask("Pick one: "))
		if n, err := strconv.Atoi(ch); err == nil && n >= 1 && n <= len(list) {
			cfg.Mode, cfg.RealmID, cfg.RealmName = "realm", list[n-1].ID, list[n-1].Name
			break
		}
		if ch == "c" {
			code := ask("Invite code or link: ")
			code = code[strings.LastIndex(code, "/")+1:]
			r, err := rc.Realm(ctx, code)
			if err != nil {
				fmt.Fprintln(out, "  That code didn't work:", err)
				continue
			}
			cfg.Mode, cfg.RealmID, cfg.RealmName = "realm", r.ID, r.Name
			break
		}
		if ch == "s" {
			addr := ask("Server address (like play.example.net or 1.2.3.4:19132): ")
			if !strings.Contains(addr, ":") {
				addr += ":19132"
			}
			cfg.Mode, cfg.Address = "server", addr
			break
		}
	}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = os.WriteFile("config.json", b, 0644)
	fmt.Fprintln(out, )
	return cfg
}

// ---------- connecting to the real server ----------

func dialTarget(ctx context.Context, cfg config, src oauth2.TokenSource, conn *minecraft.Conn) (*minecraft.Conn, func(), error) {
	cd := conn.ClientData()
	if cfg.Mode == "server" {
		c, err := minecraft.Dialer{TokenSource: src, ClientData: cd, ErrorLog: logger}.DialContext(ctx, "raknet", cfg.Address)
		return c, func() {}, err
	}

	rc := realms.NewClient(src, nil)
	fmt.Fprintln(out, "[realm] asking Realms for the address (starts the Realm if it's asleep)...")
	addr, err := rc.RealmAddress(ctx, cfg.RealmID)
	if err != nil {
		return nil, nil, fmt.Errorf("realm address: %w", err)
	}
	proto := realms.ParseNetworkProtocol(string(addr.NetworkProtocol))
	fmt.Fprintf(out, "[realm] protocol %s\n", proto)
	if proto == realms.NetworkProtocolDefault || proto == "" {
		c, err := minecraft.Dialer{TokenSource: src, ClientData: cd, ErrorLog: logger}.DialContext(ctx, "raknet", addr.Address)
		return c, func() {}, err
	}

	// NetherNet Realm: log in to Xbox + PlayFab + Minecraft services for signaling.
	xbl, err := xsapi.ClientConfig{RTAMode: xsapi.RTADisabled}.New(ctx, auth.ContextSession(ctx, src))
	if err != nil {
		return nil, nil, fmt.Errorf("xbox login: %w", err)
	}
	disc, err := service.Discover(ctx, service.ApplicationTypeMinecraftPE, protocol.CurrentVersion)
	if err != nil {
		xbl.Close()
		return nil, nil, fmt.Errorf("discover: %w", err)
	}
	env := new(service.AuthorizationEnvironment)
	if err := disc.Environment(env); err != nil {
		xbl.Close()
		return nil, nil, err
	}
	pf, err := playfab.LoginWithXbox(ctx, env.PlayFabTitleID, xbl, playfab.ClientConfig{CreateAccount: true})
	if err != nil {
		xbl.Close()
		return nil, nil, fmt.Errorf("playfab login: %w", err)
	}
	mct, err := env.TokenSource(pf, service.TokenConfig{}).ServiceToken(ctx)
	if err != nil {
		pf.Close()
		xbl.Close()
		return nil, nil, fmt.Errorf("service token: %w", err)
	}

	var sig interface {
		Close() error
	}
	var network minecraft.NetherNet
	if proto == realms.NetworkProtocolNetherNetJSONRPC {
		s, err := dialSignaling(ctx, mct, logger)
		if err != nil {
			pf.Close()
			xbl.Close()
			return nil, nil, err
		}
		sig, network.Signaling = s, s
	} else {
		s, err := dialLegacySignaling(ctx, mct, logger)
		if err != nil {
			pf.Close()
			xbl.Close()
			return nil, nil, err
		}
		sig, network.Signaling = s, s
	}
	cleanup := func() { _ = sig.Close(); pf.Close(); xbl.Close() }
	c, err := minecraft.Dialer{
		TokenSource:          src,
		ClientData:           cd,
		XBLClient:            xbl,
		PlayFabClient:        pf,
		DisablePortFollowing: true,
		ErrorLog:             logger,
	}.DialContextNetwork(ctx, network, addr.Address)
	if err != nil {
		cleanup()
		return nil, nil, err
	}
	return c, cleanup, nil
}

func handleConn(conn *minecraft.Conn, listener *minecraft.Listener, cfg config, src oauth2.TokenSource) {
	name := conn.IdentityData().DisplayName
	fmt.Fprintf(out, "[+] %s connected, joining target...\n", name)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	server, cleanup, err := dialTarget(ctx, cfg, src, conn)
	cancel()
	if err != nil {
		fmt.Fprintln(out, "[!] could not join:", err)
		_ = listener.Disconnect(conn, "FreeProxy couldn't join: "+err.Error())
		return
	}
	defer cleanup()

	var g sync.WaitGroup
	var startErr, spawnErr error
	g.Add(2)
	go func() { defer g.Done(); startErr = conn.StartGame(server.GameData()) }()
	go func() { defer g.Done(); spawnErr = server.DoSpawn() }()
	g.Wait()
	if startErr != nil || spawnErr != nil {
		fmt.Fprintln(out, "[!] spawn failed:", errors.Join(startErr, spawnErr))
		_ = listener.Disconnect(conn, "spawn failed")
		_ = server.Close()
		return
	}
	fmt.Fprintf(out, "[+] %s is in! Type .help in chat.\n", name)

	s := newSession(conn, server)
	s.chat("§aFreeProxy connected! Type §e.help§a for commands.")

	done := make(chan struct{})
	var once sync.Once
	stop := func(reason string) {
		once.Do(func() {
			_ = listener.Disconnect(conn, reason)
			_ = server.Close()
			close(done)
		})
	}
	go func() { // phone -> server
		for {
			pk, err := conn.ReadPacket()
			if err != nil {
				stop("connection lost")
				return
			}
			if !s.fromClient(pk) {
				continue
			}
			if err := server.WritePacket(pk); err != nil {
				var d minecraft.DisconnectError
				if errors.As(err, &d) {
					stop(d.Error())
				} else {
					stop("connection lost")
				}
				return
			}
		}
	}()
	go func() { // server -> phone
		for {
			pk, err := server.ReadPacket()
			if err != nil {
				var d minecraft.DisconnectError
				if errors.As(err, &d) {
					stop(d.Error())
				} else {
					stop("server connection lost")
				}
				return
			}
			if !s.fromServer(pk) {
				continue
			}
			if err := conn.WritePacket(pk); err != nil {
				stop("connection lost")
				return
			}
		}
	}()
	go s.tickLoop(done)
	<-done
	fmt.Fprintf(out, "[-] %s left.\n", name)
}

// ---------- helpers ----------

// Go programs on Android can't read the system DNS settings, so use public DNS.
func fixAndroidDNS() {
	d := net.Dialer{Timeout: 5 * time.Second}
	servers := []string{"1.1.1.1:53", "8.8.8.8:53"}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var err error
			for _, s := range servers {
				var c net.Conn
				if c, err = d.DialContext(ctx, network, s); err == nil {
					return c, nil
				}
			}
			return nil, err
		},
	}
}

func localIPs() []string {
	var out []string
	// The UDP trick finds the Wi-Fi address without needing interface permissions.
	if c, err := net.Dial("udp", "1.1.1.1:53"); err == nil {
		out = append(out, c.LocalAddr().(*net.UDPAddr).IP.String())
		c.Close()
	}
	if len(out) == 0 {
		addrs, _ := anet.InterfaceAddrs() // works on Android 11+ unlike net.Interfaces
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.To4() != nil && !ipn.IP.IsLoopback() {
				out = append(out, ipn.IP.String())
			}
		}
	}
	if len(out) == 0 {
		out = []string{"<this phone's Wi-Fi IP: Settings > About phone > Status>"}
	}
	return out
}

type fatalErr string

func (f fatalErr) Error() string { return string(f) }

func fatal(f string, a ...any) {
	panic(fatalErr(fmt.Sprintf(f, a...)))
}

// Run starts the proxy. dir is where token.json/config.json are kept.
// It blocks until the proxy stops and returns the reason.
func Run(dir string, in io.Reader, w io.Writer, args []string, listenHost string, defaultPort int) (err error) {
	stdin, out = bufio.NewReader(in), w
	if dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		if err := os.Chdir(dir); err != nil {
			return err
		}
	}
	defer func() {
		if r := recover(); r != nil {
			if f, ok := r.(fatalErr); ok {
				fmt.Fprintf(out, "\n[ERROR] %s\n", string(f))
				err = f
				return
			}
			panic(r)
		}
	}()
	runMain(args, listenHost, defaultPort)
	return nil
}
