package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iip.conf")
	content := `# Kommentar
listen_addr = ::
listen_port = 4747

data_file = /tmp/x.json
rate_limit = 2.5
rate_limit_burst = 4
max_response_size = 1200
max_connections = 7
unbekannt = egal
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	want := Config{
		ListenAddr:      "::",
		ListenPort:      4747,
		DataFile:        "/tmp/x.json",
		RateLimit:       2.5,
		RateLimitBurst:  4,
		MaxResponseSize: 1200,
		MaxConnections:  7,
	}
	if cfg != want {
		t.Errorf("Config = %+v, erwartet %+v", cfg, want)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "leer.conf")
	if err := os.WriteFile(path, []byte("# nur Kommentar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg != defaultConfig() {
		t.Errorf("leere Config sollte Defaults liefern, bekam %+v", cfg)
	}
	if cfg.ListenPort != defaultPort {
		t.Errorf("Default-Port = %d, erwartet %d", cfg.ListenPort, defaultPort)
	}
}

func TestLoadConfigInvalidPort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kaputt.conf")
	if err := os.WriteFile(path, []byte("listen_port = abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Error("ungültiger listen_port sollte einen Fehler liefern")
	}
}

func render(t *testing.T, in string) string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader([]byte(in)))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		t.Fatal(err)
	}
	s, err := buildJSONNode(dec, tok, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestHumanOutput(t *testing.T) {
	in := `{
  "hostname": "linuxmc.tech",
  "ipv6": "2001:db8::1",
  "services": [
    { "name": "tf2", "port": 27015 },
    { "name": "web", "port": 443 }
  ],
  "tags": ["a", "b"],
  "meta": { "ok": true, "none": null },
  "updated": "2026-01-01T00:00:00Z"
}`
	want := "hostname: linuxmc.tech\n" +
		"ipv6: 2001:db8::1\n" +
		"services:\n" +
		"  - name: tf2\n" +
		"    port: 27015\n" +
		"  - name: web\n" +
		"    port: 443\n" +
		"tags:\n" +
		"  - a\n" +
		"  - b\n" +
		"meta:\n" +
		"  ok: true\n" +
		"  none: null\n" +
		"updated: 2026-01-01T00:00:00Z\n"

	if got := render(t, in); got != want {
		t.Errorf("Ausgabe stimmt nicht.\n--- bekommen ---\n%s\n--- erwartet ---\n%s", got, want)
	}
}

func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(1, 3) // 1 Request/s, Burst 3

	for i := 0; i < 3; i++ {
		if !rl.Allow("192.0.2.1") {
			t.Fatalf("Request %d innerhalb des Bursts sollte erlaubt sein", i+1)
		}
	}
	if rl.Allow("192.0.2.1") {
		t.Error("Request über dem Burst sollte verworfen werden")
	}
	// Andere Quell-IP hat einen eigenen Bucket.
	if !rl.Allow("192.0.2.2") {
		t.Error("andere IP sollte nicht betroffen sein")
	}

	time.Sleep(1100 * time.Millisecond)
	if !rl.Allow("192.0.2.1") {
		t.Error("nach ~1s sollte wieder ein Token verfügbar sein")
	}
}

func TestDataStoreMaxSize(t *testing.T) {
	path := filepath.Join(t.TempDir(), "iip.json")
	if err := os.WriteFile(path, []byte(`{"a":"`+string(bytes.Repeat([]byte("x"), 100))+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ds := &DataStore{}
	if err := ds.Load(path, 50); err == nil {
		t.Error("zu große Datei sollte abgelehnt werden")
	}
	if err := ds.Load(path, 500); err != nil {
		t.Errorf("Datei innerhalb des Limits sollte geladen werden: %v", err)
	}
	if len(ds.Get()) == 0 {
		t.Error("Daten sollten nach erfolgreichem Load verfügbar sein")
	}
}

func TestDataStoreRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "iip.json")
	if err := os.WriteFile(path, []byte(`{"ok": true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ds := &DataStore{}
	if err := ds.Load(path, 500); err != nil {
		t.Fatal(err)
	}

	// Kaputte Datei darf die zuletzt gültigen Daten nicht ersetzen.
	if err := os.WriteFile(path, []byte(`{"ok": tru`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ds.Load(path, 500); err == nil {
		t.Error("ungültiges JSON sollte abgelehnt werden")
	}
	if got := string(ds.Get()); got != `{"ok": true}` {
		t.Errorf("alte Daten sollten erhalten bleiben, bekam %q", got)
	}
}

func TestIsConfigPath(t *testing.T) {
	paths := []string{"/etc/iip/iip.conf", "./iip.conf", "../iip.conf", "conf/iip.conf"}
	for _, p := range paths {
		if !isConfigPath(p) {
			t.Errorf("%q sollte als Config-Pfad gelten", p)
		}
	}
	targets := []string{"203.0.113.10", "2001:db8::1", "fe80::1%eth0", "linuxmc.tech", "localhost"}
	for _, a := range targets {
		if isConfigPath(a) {
			t.Errorf("%q sollte als Zieladresse gelten", a)
		}
	}
}

func TestListenFamily(t *testing.T) {
	cases := map[string]string{
		"0.0.0.0":        "4",
		"127.0.0.1":      "4",
		"::ffff:1.2.3.4": "4",
		"::":             "6",
		"::1":            "6",
		"2001:db8::1":    "6",
	}
	for in, want := range cases {
		if got := listenFamily(net.ParseIP(in)); got != want {
			t.Errorf("listenFamily(%s) = %s, erwartet %s", in, got, want)
		}
	}
}

// ---------- Integrationstests: TCP + UDP auf demselben Port ----------

const testData = `{"hostname":"test.example","ipv6":"2001:db8::1"}`

func TestMain(m *testing.M) {
	tcpIOTimeout = 500 * time.Millisecond // Tests sollen nicht auf 5 s Timeouts warten
	os.Exit(m.Run())
}

// startTestServer startet TCP- und UDP-Listener auf demselben freien Loopback-Port.
func startTestServer(t *testing.T, maxConns int, rate float64, burst int) int {
	t.Helper()
	for attempt := 0; attempt < 50; attempt++ {
		tcpLn, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		port := tcpLn.Addr().(*net.TCPAddr).Port
		udpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
		if err != nil {
			tcpLn.Close() // UDP-Port zufällig belegt, nächster Versuch
			continue
		}
		store := &DataStore{bytes: []byte(testData)}
		limiter := NewRateLimiter(rate, burst)
		go serveTCP(tcpLn, store, limiter, maxConns)
		go serveUDP(udpConn, store, limiter)
		t.Cleanup(func() { tcpLn.Close(); udpConn.Close() })
		return port
	}
	t.Fatal("kein Port gefunden, der für TCP und UDP frei ist")
	return 0
}

func TestTCPAndUDPOnSamePort(t *testing.T) {
	port := startTestServer(t, 10, 100, 100)

	got, err := fetchTCP("127.0.0.1", port, 2*time.Second)
	if err != nil {
		t.Fatalf("fetchTCP: %v", err)
	}
	if string(got) != testData {
		t.Errorf("TCP-Antwort = %q, erwartet %q", got, testData)
	}

	got, err = fetchUDP("127.0.0.1", port, 2*time.Second)
	if err != nil {
		t.Fatalf("fetchUDP: %v", err)
	}
	if string(got) != testData {
		t.Errorf("UDP-Antwort = %q, erwartet %q", got, testData)
	}
}

func TestTCPRequestVariants(t *testing.T) {
	port := startTestServer(t, 10, 100, 100)
	addr := net.JoinHostPort("127.0.0.1", itoa(port))

	roundTrip := func(payload string, closeWrite bool) string {
		t.Helper()
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := conn.Write([]byte(payload)); err != nil {
			t.Fatal(err)
		}
		if closeWrite {
			conn.(*net.TCPConn).CloseWrite()
		}
		b, _ := io.ReadAll(conn)
		return string(b)
	}

	if got := roundTrip("get\n", false); got != testData {
		t.Errorf("klein geschriebenes get + Zeilenumbruch: %q", got)
	}
	if got := roundTrip("GET\r\n", false); got != testData {
		t.Errorf("GET mit CRLF: %q", got)
	}
	if got := roundTrip("GET", true); got != testData {
		t.Errorf("GET ohne Zeilenumbruch, Sende-Richtung geschlossen: %q", got)
	}
	if got := roundTrip("HELLO\n", false); got != "" {
		t.Errorf("falsche Anfrage sollte ohne Antwort bleiben, bekam %q", got)
	}
}

func TestUDPIgnoresInvalidRequest(t *testing.T) {
	port := startTestServer(t, 10, 100, 100)
	conn, err := net.Dial("udp", net.JoinHostPort("127.0.0.1", itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	conn.Write([]byte("HELLO"))
	buf := make([]byte, 100)
	if n, err := conn.Read(buf); err == nil {
		t.Errorf("falsche UDP-Anfrage sollte ohne Antwort bleiben, bekam %q", buf[:n])
	}
}

func TestTCPMaxConnections(t *testing.T) {
	port := startTestServer(t, 1, 100, 100)
	addr := net.JoinHostPort("127.0.0.1", itoa(port))

	// Verbindung 1 belegt den einzigen Platz, ohne etwas zu senden.
	hold, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Close()
	time.Sleep(100 * time.Millisecond)

	// Verbindung 2 wird vom Server sofort geschlossen.
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if n, err := second.Read(make([]byte, 10)); err != io.EOF || n != 0 {
		t.Errorf("zweite Verbindung sollte geschlossen werden (EOF), bekam n=%d err=%v", n, err)
	}

	// Nach dem Timeout der ersten Verbindung ist der Platz wieder frei.
	time.Sleep(tcpIOTimeout + 300*time.Millisecond)
	if _, err := fetchTCP("127.0.0.1", port, 2*time.Second); err != nil {
		t.Errorf("nach Freigabe des Platzes sollte eine Abfrage klappen: %v", err)
	}
}

func TestTCPRateLimit(t *testing.T) {
	port := startTestServer(t, 10, 0.001, 2) // praktisch nur der Burst von 2

	for i := 0; i < 2; i++ {
		if _, err := fetchTCP("127.0.0.1", port, 2*time.Second); err != nil {
			t.Fatalf("Abfrage %d innerhalb des Bursts: %v", i+1, err)
		}
	}
	if _, err := fetchTCP("127.0.0.1", port, 2*time.Second); err == nil {
		t.Error("dritte Abfrage sollte durch das Rate-Limit ohne Antwort bleiben")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
