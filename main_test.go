package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
