// iip - Internet Informations Protokoll Daemon
//
// Simples UDP-Protokoll: Client sendet "GET" an Port <listen_port>/UDP,
// Server antwortet mit dem Inhalt von /etc/iip/iip.json (öffentliche,
// nicht-vertrauliche Server-Infos wie IPv6-Adresse, laufende Services etc.).
//
// Bewusst ohne Authentifizierung - die Daten sind für die Öffentlichkeit
// gedacht. Ein einfaches Rate-Limiting pro Quell-IP schützt vor Missbrauch
// als UDP-Amplification-Vektor.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
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

// defaultPort ist der Standard-Port von Server und Client. Er liegt im
// Dynamic/Private-Bereich (49152-65535) und ist daher nicht bei IANA registrierbar.
const defaultPort = 65535

// ---------- Config ----------

type Config struct {
	ListenAddr      string
	ListenPort      int
	DataFile        string
	RateLimit       float64 // erlaubte Requests pro Sekunde und Quell-IP
	RateLimitBurst  int     // Burst-Kapazität
	MaxResponseSize int     // Sicherheitslimit in Bytes
}

func defaultConfig() Config {
	return Config{
		ListenAddr:      "0.0.0.0",
		ListenPort:      defaultPort,
		DataFile:        "/etc/iip/iip.json",
		RateLimit:       5,
		RateLimitBurst:  10,
		MaxResponseSize: 8192,
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

// ---------- Client-Modus ----------

// runClient sendet "GET" an target:port/udp und gibt die Antwort aus.
// target kann eine IPv4-, IPv6-Adresse oder ein Hostname sein.
// Bei raw=true wird die JSON-Antwort unverändert ausgegeben, sonst
// in einem einfachen, gut lesbaren "key: value"-Format.
func runClient(target string, port int, timeout time.Duration, raw bool) error {
	raddr, err := net.ResolveUDPAddr("udp", net.JoinHostPort(target, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("Adresse %q konnte nicht aufgelöst werden: %w", target, err)
	}

	conn, err := net.DialUDP("udp", nil, raddr)
	if err != nil {
		return fmt.Errorf("Verbindung zu %s fehlgeschlagen: %w", raddr, err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte("GET")); err != nil {
		return fmt.Errorf("Senden fehlgeschlagen: %w", err)
	}

	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}

	buf := make([]byte, 65535)
	n, err := conn.Read(buf)
	if err != nil {
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			return fmt.Errorf("keine Antwort von %s innerhalb von %s (Timeout)", raddr, timeout)
		}
		return fmt.Errorf("Lesen der Antwort fehlgeschlagen: %w", err)
	}

	if raw {
		fmt.Println(string(buf[:n]))
		return nil
	}
	return printHuman(buf[:n])
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

func main() {
	// Flags und Positionsargumente trennen (einfaches manuelles Parsing reicht hier).
	rawJSON := false
	var positional []string
	for _, a := range os.Args[1:] {
		if a == "-json" || a == "--json" {
			rawJSON = true
			continue
		}
		positional = append(positional, a)
	}

	// Client-Modus: ein erster Parameter, der kein Dateipfad ist, gilt als
	// Zieladresse, z.B.  iip 2001:a22:34::1  oder  iip 2001:a22:34::1 65535 -json
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
		if err := runClient(target, port, 3*time.Second, rawJSON); err != nil {
			log.Fatal(err)
		}
		return
	}

	confPath := "/etc/iip/iip.conf"
	if len(positional) > 0 {
		confPath = positional[0]
	}

	cfg, err := loadConfig(confPath)
	if err != nil {
		log.Fatalf("Konfiguration konnte nicht geladen werden (%s): %v", confPath, err)
	}
	log.Printf("Konfiguration geladen: %s (Listen %s:%d, Datenfile %s)",
		confPath, cfg.ListenAddr, cfg.ListenPort, cfg.DataFile)

	store := &DataStore{}
	if err := store.Load(cfg.DataFile, cfg.MaxResponseSize); err != nil {
		log.Fatalf("Datendatei konnte nicht geladen werden (%s): %v", cfg.DataFile, err)
	}

	// SIGHUP: iip.json neu einlesen, ohne Neustart. Dabei werden data_file und
	// max_response_size aus der Config neu gelesen; Listen-Adresse, Port und
	// Rate-Limit werden nur beim Start ausgewertet (dafür ist ein Neustart nötig).
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

	addr := net.UDPAddr{
		IP:   net.ParseIP(cfg.ListenAddr),
		Port: cfg.ListenPort,
	}

	// "udp" (ohne Suffix) wuerde bei einer Wildcard-Adresse (0.0.0.0 ODER ::)
	// von Go automatisch zu einem Dual-Stack-IPv6-Socket, der auch IPv4-mapped
	// Verbindungen annimmt - unabhaengig davon, welche der beiden Wildcards
	// konfiguriert ist. Damit listen_addr in der Config wirklich die
	// Adressfamilie festlegt, binden wir explizit per udp4/udp6.
	network := "udp"
	if addr.IP.To4() != nil {
		network = "udp4"
	} else if addr.IP.To16() != nil {
		network = "udp6"
	}

	conn, err := net.ListenUDP(network, &addr)
	if err != nil {
		log.Fatalf("Konnte UDP-Socket nicht öffnen (%s:%d): %v", cfg.ListenAddr, cfg.ListenPort, err)
	}
	defer conn.Close()
	log.Printf("iip lauscht auf %s:%d/udp", cfg.ListenAddr, cfg.ListenPort)

	limiter := NewRateLimiter(cfg.RateLimit, cfg.RateLimitBurst)
	buf := make([]byte, 512) // Requests sind nur "GET", mehr brauchen wir nicht

	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("Lesefehler: %v", err)
			continue
		}

		ip := remoteAddr.IP.String()
		if !limiter.Allow(ip) {
			continue // stillschweigend verwerfen, keine Fehlerantwort an unbekannte Absender
		}

		req := strings.TrimSpace(string(buf[:n]))
		if !strings.EqualFold(req, "GET") {
			continue // nur exakt "GET" wird beantwortet
		}

		data := store.Get()
		if _, err := conn.WriteToUDP(data, remoteAddr); err != nil {
			log.Printf("Antwort an %s fehlgeschlagen: %v", remoteAddr, err)
		}
	}
}
