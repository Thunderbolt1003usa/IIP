// iip - Internet Informations Protokoll
//
// Einfaches Abfrageprotokoll für öffentliche, nicht vertrauliche Server-Infos
// (IP-Adressen, Organisation, öffentliche Dienste ...). Eine Binary ist Server
// und Client zugleich.
//
// Der Server lauscht auf demselben Port per TCP und per UDP und beantwortet
// immer beides:
//
//	TCP: Client sendet "GET\n", Server schickt den Inhalt von iip.json und
//	     schließt die Verbindung.
//	UDP: Client sendet ein Datagramm "GET", Server antwortet mit einem
//	     Datagramm mit dem Inhalt von iip.json.
//
// Der Client nutzt standardmäßig TCP, per -udp wird UDP verwendet.
//
// Bewusst ohne Authentifizierung - die Daten sind für die Öffentlichkeit
// gedacht. Ein Rate-Limit pro Quell-IP begrenzt Missbrauch, bei UDP vor allem
// als Reflection-/Amplification-Vektor.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// defaultPort ist der Standard-Port von Server und Client (TCP und UDP). Er liegt
// im Dynamic/Private-Bereich (49152-65535) und ist daher nicht bei IANA registrierbar.
const defaultPort = 65535

const (
	maxRequestSize    = 512     // Anfragen sind nur "GET", mehr wird nicht gelesen
	maxClientResponse = 1 << 20 // Obergrenze für TCP-Antworten im Client (1 MiB)
)

// tcpIOTimeout begrenzt Lesen der Anfrage und Schreiben der Antwort pro
// TCP-Verbindung (verhindert, dass langsame Clients Verbindungsplätze blockieren).
// Variable statt Konstante, damit Tests sie verkürzen können.
var tcpIOTimeout = 5 * time.Second

// ---------- Config ----------

type Config struct {
	ListenAddr      string
	ListenPort      int
	DataFile        string
	RateLimit       float64 // erlaubte Requests pro Sekunde und Quell-IP (TCP + UDP zusammen)
	RateLimitBurst  int     // Burst-Kapazität
	MaxResponseSize int     // Sicherheitslimit in Bytes
	MaxConnections  int     // maximal gleichzeitige TCP-Verbindungen
}

func defaultConfig() Config {
	return Config{
		ListenAddr:      "0.0.0.0",
		ListenPort:      defaultPort,
		DataFile:        "/etc/iip/iip.json",
		RateLimit:       5,
		RateLimitBurst:  10,
		MaxResponseSize: 8192,
		MaxConnections:  100,
	}
}

// loadConfig liest eine einfache key=value Datei (Kommentare mit #, leere Zeilen ok).
func loadConfig(path string) (Config, error) {
	cfg := defaultConfig()

	f, err := os.Open(path)
	if err != nil {
		return cfg, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			log.Printf("iip.conf Zeile %d ignoriert (kein key=value): %q", lineNo, line)
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])

		switch key {
		case "listen_addr":
			cfg.ListenAddr = val
		case "listen_port":
			p, err := strconv.Atoi(val)
			if err != nil {
				return cfg, fmt.Errorf("ungültiger listen_port in Zeile %d: %v", lineNo, err)
			}
			cfg.ListenPort = p
		case "data_file":
			cfg.DataFile = val
		case "rate_limit":
			r, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return cfg, fmt.Errorf("ungültiger rate_limit in Zeile %d: %v", lineNo, err)
			}
			cfg.RateLimit = r
		case "rate_limit_burst":
			b, err := strconv.Atoi(val)
			if err != nil {
				return cfg, fmt.Errorf("ungültiger rate_limit_burst in Zeile %d: %v", lineNo, err)
			}
			cfg.RateLimitBurst = b
		case "max_response_size":
			m, err := strconv.Atoi(val)
			if err != nil {
				return cfg, fmt.Errorf("ungültiger max_response_size in Zeile %d: %v", lineNo, err)
			}
			cfg.MaxResponseSize = m
		case "max_connections":
			c, err := strconv.Atoi(val)
			if err != nil {
				return cfg, fmt.Errorf("ungültiger max_connections in Zeile %d: %v", lineNo, err)
			}
			cfg.MaxConnections = c
		default:
			log.Printf("iip.conf Zeile %d: unbekannter Key %q ignoriert", lineNo, key)
		}
	}
	return cfg, scanner.Err()
}

// ---------- Datendatei (iip.json) ----------

type DataStore struct {
	mu    sync.RWMutex
	bytes []byte
}

func (d *DataStore) Get() []byte {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.bytes
}

func (d *DataStore) Load(path string, maxSize int) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(b) > maxSize {
		return fmt.Errorf("iip.json (%d Bytes) überschreitet max_response_size (%d Bytes)", len(b), maxSize)
	}
	// Ungültiges JSON nie ausliefern. Bei einem fehlgeschlagenen Reload (SIGHUP)
	// bleiben so die zuletzt gültigen Daten aktiv.
	if !json.Valid(b) {
		return fmt.Errorf("%s enthält kein gültiges JSON", path)
	}
	d.mu.Lock()
	d.bytes = b
	d.mu.Unlock()
	return nil
}

// ---------- Rate Limiting (Token Bucket pro Quell-IP) ----------

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   int
}

func NewRateLimiter(rate float64, burst int) *RateLimiter {
	rl := &RateLimiter{
		buckets: make(map[string]*bucket),
		rate:    rate,
		burst:   burst,
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) Allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	b, ok := rl.buckets[ip]
	if !ok {
		b = &bucket{tokens: float64(rl.burst), lastSeen: now}
		rl.buckets[ip] = b
	}

	elapsed := now.Sub(b.lastSeen).Seconds()
	b.lastSeen = now
	b.tokens += elapsed * rl.rate
	if b.tokens > float64(rl.burst) {
		b.tokens = float64(rl.burst)
	}

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// cleanupLoop entfernt alte, inaktive Buckets, damit die Map nicht unbegrenzt wächst.
func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		rl.mu.Lock()
		cutoff := time.Now().Add(-10 * time.Minute)
		for ip, b := range rl.buckets {
			if b.lastSeen.Before(cutoff) {
				delete(rl.buckets, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// ---------- Server (TCP + UDP) ----------

// remoteIP liefert die Quell-IP einer Verbindung als String (für das Rate-Limit).
func remoteIP(a net.Addr) string {
	if ta, ok := a.(*net.TCPAddr); ok {
		return ta.IP.String()
	}
	host, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return host
}

// serveUDP beantwortet UDP-Datagramme "GET" mit dem Inhalt der Datendatei.
// Kehrt erst zurück, wenn der Socket geschlossen wird.
func serveUDP(conn *net.UDPConn, store *DataStore, limiter *RateLimiter) {
	buf := make([]byte, maxRequestSize)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("UDP Lesefehler: %v", err)
			continue
		}

		if !limiter.Allow(remoteAddr.IP.String()) {
			continue // stillschweigend verwerfen, keine Fehlerantwort an unbekannte Absender
		}

		req := strings.TrimSpace(string(buf[:n]))
		if !strings.EqualFold(req, "GET") {
			continue // nur exakt "GET" wird beantwortet
		}

		if _, err := conn.WriteToUDP(store.Get(), remoteAddr); err != nil {
			log.Printf("UDP Antwort an %s fehlgeschlagen: %v", remoteAddr, err)
		}
	}
}

// serveTCP nimmt TCP-Verbindungen an. Jede Verbindung wird in einer eigenen
// Goroutine bedient; gleichzeitig sind höchstens maxConns Verbindungen offen,
// weitere werden sofort geschlossen. Kehrt zurück, wenn der Listener geschlossen wird.
func serveTCP(ln net.Listener, store *DataStore, limiter *RateLimiter, maxConns int) {
	sem := make(chan struct{}, maxConns)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("TCP Accept-Fehler: %v", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}

		if !limiter.Allow(remoteIP(conn.RemoteAddr())) {
			conn.Close()
			continue
		}

		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				handleTCP(conn, store)
			}()
		default:
			conn.Close() // alle Verbindungsplätze belegt
		}
	}
}

// handleTCP liest eine Zeile (oder bis zum Ende des Streams), erwartet "GET",
// schickt die Daten und schließt die Verbindung. Alles andere wird ohne Antwort
// verworfen. Die Anfrage darf also mit Zeilenumbruch oder mit geschlossener
// Sende-Richtung enden (echo GET | nc ... bzw. printf GET | nc -N ...).
func handleTCP(conn net.Conn, store *DataStore) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(tcpIOTimeout))

	br := bufio.NewReaderSize(io.LimitReader(conn, maxRequestSize), maxRequestSize)
	line, err := br.ReadString('\n')
	if err != nil && line == "" {
		return // nichts gelesen (Verbindung zu, Timeout, Reset)
	}
	if !strings.EqualFold(strings.TrimSpace(line), "GET") {
		return
	}

	if _, err := conn.Write(store.Get()); err != nil {
		log.Printf("TCP Antwort an %s fehlgeschlagen: %v", conn.RemoteAddr(), err)
		return
	}
	if tc, ok := conn.(*net.TCPConn); ok {
		_ = tc.CloseWrite() // sauberes FIN nach den Daten
	}
}

// listenFamily gibt "4" oder "6" zurück, passend zur Adressfamilie von ip.
// Damit wird das Netzwerk explizit gewählt ("tcp4"/"udp4" bzw. "tcp6"/"udp6"):
// Mit dem generischen "tcp"/"udp" würde Go bei einer Wildcard-Adresse
// (0.0.0.0 ODER ::) automatisch einen Dual-Stack-IPv6-Socket öffnen.
func listenFamily(ip net.IP) string {
	if ip.To4() != nil {
		return "4"
	}
	return "6"
}

// runServer lädt die Konfiguration und startet TCP- und UDP-Listener auf
// demselben Port. Kehrt nur bei einem Fehler zurück.
func runServer(confPath string) error {
	cfg, err := loadConfig(confPath)
	if err != nil {
		return fmt.Errorf("Konfiguration konnte nicht geladen werden (%s): %w", confPath, err)
	}
	log.Printf("Konfiguration geladen: %s (Listen %s:%d, Datenfile %s)",
		confPath, cfg.ListenAddr, cfg.ListenPort, cfg.DataFile)

	ip := net.ParseIP(cfg.ListenAddr)
	if ip == nil {
		return fmt.Errorf("listen_addr %q ist keine gültige IP-Adresse", cfg.ListenAddr)
	}
	if cfg.ListenPort < 1 || cfg.ListenPort > 65535 {
		return fmt.Errorf("listen_port %d liegt außerhalb von 1-65535", cfg.ListenPort)
	}
	if cfg.MaxConnections < 1 {
		return fmt.Errorf("max_connections muss mindestens 1 sein")
	}

	store := &DataStore{}
	if err := store.Load(cfg.DataFile, cfg.MaxResponseSize); err != nil {
		return fmt.Errorf("Datendatei konnte nicht geladen werden (%s): %w", cfg.DataFile, err)
	}

	// SIGHUP: iip.json neu einlesen, ohne Neustart. Dabei werden data_file und
	// max_response_size aus der Config neu gelesen; Listen-Adresse, Port,
	// Rate-Limit und max_connections werden nur beim Start ausgewertet
	// (dafür ist ein Neustart nötig).
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			newCfg, err := loadConfig(confPath)
			if err != nil {
				log.Printf("Reload fehlgeschlagen (Config): %v", err)
				continue
			}
			if err := store.Load(newCfg.DataFile, newCfg.MaxResponseSize); err != nil {
				log.Printf("Reload fehlgeschlagen (Daten): %v", err)
				continue
			}
			log.Printf("Daten neu geladen (SIGHUP): %s", newCfg.DataFile)
		}
	}()

	fam := listenFamily(ip)
	tcpLn, err := net.ListenTCP("tcp"+fam, &net.TCPAddr{IP: ip, Port: cfg.ListenPort})
	if err != nil {
		return fmt.Errorf("TCP-Socket konnte nicht geöffnet werden (%s:%d): %w", cfg.ListenAddr, cfg.ListenPort, err)
	}
	defer tcpLn.Close()
	udpConn, err := net.ListenUDP("udp"+fam, &net.UDPAddr{IP: ip, Port: cfg.ListenPort})
	if err != nil {
		return fmt.Errorf("UDP-Socket konnte nicht geöffnet werden (%s:%d): %w", cfg.ListenAddr, cfg.ListenPort, err)
	}
	defer udpConn.Close()
	log.Printf("iip lauscht auf %s (tcp+udp)", net.JoinHostPort(cfg.ListenAddr, strconv.Itoa(cfg.ListenPort)))

	limiter := NewRateLimiter(cfg.RateLimit, cfg.RateLimitBurst)
	go serveTCP(tcpLn, store, limiter, cfg.MaxConnections)
	serveUDP(udpConn, store, limiter)
	return errors.New("UDP-Socket wurde unerwartet geschlossen")
}

// ---------- Client-Modus ----------

// fetchTCP fragt einen IIP-Server per TCP ab und gibt die rohe Antwort zurück.
// target kann eine IPv4-, IPv6-Adresse oder ein Hostname sein. Bei Hostnamen
// probiert Go mehrere Adressen durch, falls die erste nicht erreichbar ist.
func fetchTCP(target string, port int, timeout time.Duration) ([]byte, error) {
	addr := net.JoinHostPort(target, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)

	d := net.Dialer{Deadline: deadline}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("Verbindung zu %s fehlgeschlagen: %w", addr, err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}

	if _, err := conn.Write([]byte("GET\n")); err != nil {
		return nil, fmt.Errorf("Senden fehlgeschlagen: %w", err)
	}

	data, err := io.ReadAll(io.LimitReader(conn, maxClientResponse))
	if err != nil {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return nil, fmt.Errorf("keine vollständige Antwort von %s innerhalb von %s (Timeout)", addr, timeout)
		}
		return nil, fmt.Errorf("Lesen der Antwort fehlgeschlagen: %w", err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%s hat die Verbindung ohne Antwort geschlossen (Rate-Limit oder Verbindungslimit?)", addr)
	}
	return data, nil
}

// fetchUDP fragt einen IIP-Server per UDP ab und gibt die rohe Antwort zurück.
func fetchUDP(target string, port int, timeout time.Duration) ([]byte, error) {
	raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(target, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("Adresse %q konnte nicht aufgelöst werden: %w", target, err)
	}

	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return nil, fmt.Errorf("Verbindung zu %s fehlgeschlagen: %w", raddr, err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("GET")); err != nil {
		return nil, fmt.Errorf("Senden fehlgeschlagen: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}

	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			return nil, fmt.Errorf("keine Antwort von %s innerhalb von %s (Timeout)", raddr, timeout)
		}
		return nil, fmt.Errorf("Lesen der Antwort fehlgeschlagen: %w", err)
	}
	return buf[:n], nil
}

// runClient fragt target:port ab (TCP, mit useUDP per UDP) und gibt die Antwort aus.
// Bei raw=true wird die JSON-Antwort unverändert ausgegeben, sonst in einem
// einfachen, gut lesbaren "key: value"-Format.
func runClient(target string, port int, timeout time.Duration, useUDP, raw bool) error {
	var (
		data []byte
		err  error
	)
	if useUDP {
		data, err = fetchUDP(target, port, timeout)
	} else {
		data, err = fetchTCP(target, port, timeout)
	}
	if err != nil {
		return err
	}

	if raw {
		os.Stdout.Write(data)
		if len(data) == 0 || data[len(data)-1] != '\n' {
			fmt.Println()
		}
		return nil
	}
	return printHuman(data)
}

// ---------- Menschenlesbare Ausgabe ----------

// printHuman gibt beliebige JSON-Daten in einem einfachen, YAML-ähnlichen
// "key: value"-Format aus, in der Original-Feldreihenfolge des JSON.
func printHuman(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("Antwort ist kein gültiges JSON: %w", err)
	}
	s, err := buildJSONNode(dec, tok, 0, "")
	if err != nil {
		return err
	}
	fmt.Print(s)
	return nil
}

// buildJSONNode rendert einen JSON-Wert als eingerückten Text.
// label ist der Feldname (leer bei Top-Level-Objekt oder Array-Elementen ohne eigenen Key).
func buildJSONNode(dec *json.Decoder, tok json.Token, indent int, label string) (string, error) {
	pad := strings.Repeat("  ", indent)

	delim, isDelim := tok.(json.Delim)
	if !isDelim {
		if label != "" {
			return fmt.Sprintf("%s%s: %s\n", pad, label, formatScalar(tok)), nil
		}
		return fmt.Sprintf("%s%s\n", pad, formatScalar(tok)), nil
	}

	var out strings.Builder
	childIndent := indent
	if label != "" {
		out.WriteString(fmt.Sprintf("%s%s:\n", pad, label))
		childIndent++
	}

	switch delim {
	case '{':
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return "", err
			}
			key, _ := keyTok.(string)
			valTok, err := dec.Token()
			if err != nil {
				return "", err
			}
			s, err := buildJSONNode(dec, valTok, childIndent, key)
			if err != nil {
				return "", err
			}
			out.WriteString(s)
		}
		if _, err := dec.Token(); err != nil { // schliessende '}'
			return "", err
		}
	case '[':
		for dec.More() {
			valTok, err := dec.Token()
			if err != nil {
				return "", err
			}
			// Element eine Ebene tiefer rendern, dann erste Zeile mit "- " markieren.
			s, err := buildJSONNode(dec, valTok, childIndent+1, "")
			if err != nil {
				return "", err
			}
			out.WriteString(listItemPrefix(s, childIndent))
		}
		if _, err := dec.Token(); err != nil { // schliessende ']'
			return "", err
		}
	}
	return out.String(), nil
}

// listItemPrefix ersetzt die führende Einrückung der ersten Zeile durch "- ",
// sodass Listenelemente wie in YAML dargestellt werden (Folgezeilen bleiben
// gleich weit eingerückt, da "- " genauso breit ist wie eine Einrückungsstufe).
func listItemPrefix(s string, itemIndent int) string {
	full := strings.Repeat("  ", itemIndent+1)
	marker := strings.Repeat("  ", itemIndent) + "- "
	idx := strings.IndexByte(s, '\n')
	if idx == -1 {
		idx = len(s)
	} else {
		idx++
	}
	firstLine := s[:idx]
	rest := s[idx:]
	if strings.HasPrefix(firstLine, full) {
		firstLine = marker + firstLine[len(full):]
	}
	return firstLine + rest
}

func formatScalar(tok json.Token) string {
	switch v := tok.(type) {
	case nil:
		return "null"
	case bool:
		if v {
			return "true"
		}
		return "false"
	case json.Number:
		return v.String()
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

// ---------- Main ----------

// isConfigPath unterscheidet Config-Pfade von Zieladressen: Hostnamen, IPv4- und
// IPv6-Adressen enthalten nie ein "/", Dateipfade (/etc/iip/iip.conf,
// ./iip.conf, conf/iip.conf) praktisch immer.
func isConfigPath(arg string) bool {
	return strings.Contains(arg, "/")
}

const usage = `iip - Internet Informations Protokoll

Client:  iip [Optionen] <Adresse> [Port]
Server:  iip [/pfad/zur/iip.conf]          (Standard: /etc/iip/iip.conf)

Client-Optionen:
  -udp    per UDP abfragen (Standard ist TCP)
  -tcp    per TCP abfragen (Standard, nur zur Verdeutlichung)
  -json   rohe JSON-Antwort ausgeben statt "key: value"

Der Standardport ist 65535. Ein Argument mit "/" gilt als Config-Pfad.
`

func main() {
	var (
		rawJSON, useUDP, useTCP bool
		positional              []string
	)
	for _, a := range os.Args[1:] {
		switch a {
		case "-h", "-help", "--help":
			fmt.Print(usage)
			return
		case "-json", "--json":
			rawJSON = true
		case "-udp", "--udp":
			useUDP = true
		case "-tcp", "--tcp":
			useTCP = true
		default:
			if strings.HasPrefix(a, "-") {
				log.Fatalf("Unbekannte Option %q (siehe iip -h)", a)
			}
			positional = append(positional, a)
		}
	}
	if useUDP && useTCP {
		log.Fatal("-udp und -tcp schließen sich gegenseitig aus")
	}

	// Client-Modus: ein erster Parameter, der kein Dateipfad ist, gilt als
	// Zieladresse, z.B.  iip 2001:a22:34::1  oder  iip -udp 2001:a22:34::1 65535
	if len(positional) > 0 && !isConfigPath(positional[0]) {
		target := positional[0]
		port := defaultPort
		if len(positional) > 1 {
			p, err := strconv.Atoi(positional[1])
			if err != nil {
				log.Fatalf("Ungültiger Port %q: %v", positional[1], err)
			}
			port = p
		}
		if err := runClient(target, port, 3*time.Second, useUDP, rawJSON); err != nil {
			log.Fatal(err)
		}
		return
	}

	// Server-Modus
	if rawJSON || useUDP || useTCP {
		log.Fatal("-json, -udp und -tcp gelten nur im Client-Modus (Zieladresse angeben)")
	}
	confPath := "/etc/iip/iip.conf"
	if len(positional) > 0 {
		confPath = positional[0]
	}
	if err := runServer(confPath); err != nil {
		log.Fatal(err)
	}
}
