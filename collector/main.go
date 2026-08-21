// modem-exporter: scrapes an Askey/NET-DK DOCSIS cable modem (Play/Liberty
// Global firmware) and exposes Prometheus metrics + pushes the event log to
// Loki.
//
// Protocol (reverse-engineered from the modem's own JS):
//   login: POST /xml/setter.xml  token=<sessionToken cookie>&fun=15&Username=NULL&Password=sha256(pw)
//   data : POST /xml/getter.xml  token=<sessionToken cookie>&fun=N
//          fun=10 downstream, fun=11 upstream, fun=13 event log, fun=1 global (AccessLevel)
// Every response rotates the sessionToken cookie; it must be echoed as the
// `token` form field on the next request.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ---------------------------------------------------------------------------
// Config
// ---------------------------------------------------------------------------

type Config struct {
	ModemURL    string
	Password    string
	Poll        time.Duration
	LokiURL     string // empty => Loki push disabled
	ListenAddr  string
	BackoffBase time.Duration // first re-login delay after a failure
	BackoffMax  time.Duration // cap on the exponential re-login delay
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	poll, err := time.ParseDuration(envOr("POLL_INTERVAL", "30s"))
	if err != nil {
		log.Fatalf("invalid POLL_INTERVAL: %v", err)
	}
	bBase, err := time.ParseDuration(envOr("LOGIN_BACKOFF_BASE", "60s"))
	if err != nil {
		log.Fatalf("invalid LOGIN_BACKOFF_BASE: %v", err)
	}
	bMax, err := time.ParseDuration(envOr("LOGIN_BACKOFF_MAX", "15m"))
	if err != nil {
		log.Fatalf("invalid LOGIN_BACKOFF_MAX: %v", err)
	}
	c := Config{
		ModemURL:    strings.TrimRight(envOr("MODEM_URL", "http://192.168.0.1"), "/"),
		Password:    os.Getenv("MODEM_PASSWORD"),
		Poll:        poll,
		LokiURL:     strings.TrimRight(os.Getenv("LOKI_URL"), "/"),
		ListenAddr:  envOr("LISTEN_ADDR", ":9210"),
		BackoffBase: bBase,
		BackoffMax:  bMax,
	}
	if c.Password == "" {
		log.Fatal("MODEM_PASSWORD is required")
	}
	return c
}

// ---------------------------------------------------------------------------
// XML models
// ---------------------------------------------------------------------------

type dsChan struct {
	Freq         string `xml:"freq"`
	Pow          string `xml:"pow"`
	SNR          string `xml:"snr"`
	Mod          string `xml:"mod"`
	ChID         string `xml:"chid"`
	RxMER        string `xml:"RxMER"`
	PreRs        string `xml:"PreRs"`
	PostRs       string `xml:"PostRs"`
	IsQamLocked  string `xml:"IsQamLocked"`
	IsFECLocked  string `xml:"IsFECLocked"`
	IsMpegLocked string `xml:"IsMpegLocked"`
}

type usChan struct {
	Freq        string `xml:"freq"`
	Power       string `xml:"power"`
	Mod         string `xml:"mod"`
	SRate       string `xml:"srate"`
	USID        string `xml:"usid"`
	T1Timeouts  string `xml:"t1Timeouts"`
	T2Timeouts  string `xml:"t2Timeouts"`
	T3Timeouts  string `xml:"t3Timeouts"`
	T4Timeouts  string `xml:"t4Timeouts"`
	MessageType string `xml:"messageType"`
}

type eventLog struct {
	Prior string `xml:"prior"`
	Time  string `xml:"time"`
	Text  string `xml:"text"`
}

// decodeElements walks the XML at any depth and decodes every element whose
// local name matches `name` into a fresh T, calling fn for each. This is robust
// to whatever wrapper element the firmware nests the rows under.
func decodeElements[T any](data []byte, name string, fn func(T)) error {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == name {
			var v T
			if err := dec.DecodeElement(&v, &se); err != nil {
				return err
			}
			fn(v)
		}
	}
}

// firstTagValue returns the text of the first <name>...</name> in data.
func firstTagValue(data []byte, name string) string {
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false
	for {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == name {
			var s string
			if dec.DecodeElement(&s, &se) == nil {
				return strings.TrimSpace(s)
			}
		}
	}
}

func pf(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	// tolerate a trailing unit like "-4 dBmV"
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ---------------------------------------------------------------------------
// Metrics
// ---------------------------------------------------------------------------

type Metrics struct {
	up            prometheus.Gauge
	scrapeSeconds prometheus.Gauge
	loginTotal    prometheus.Counter
	loginFailed   prometheus.Counter

	dsFreq   *prometheus.GaugeVec
	dsPower  *prometheus.GaugeVec
	dsSNR    *prometheus.GaugeVec
	dsRxMER  *prometheus.GaugeVec
	dsLocked *prometheus.GaugeVec
	dsPreRaw *prometheus.GaugeVec
	dsPstRaw *prometheus.GaugeVec
	dsPre    *prometheus.CounterVec
	dsPst    *prometheus.CounterVec

	usFreq   *prometheus.GaugeVec
	usPower  *prometheus.GaugeVec
	usSRate  *prometheus.GaugeVec
	usT1Raw  *prometheus.GaugeVec
	usT2Raw  *prometheus.GaugeVec
	usT3Raw  *prometheus.GaugeVec
	usT4Raw  *prometheus.GaugeVec
	usT1     *prometheus.CounterVec
	usT2     *prometheus.CounterVec
	usT3     *prometheus.CounterVec
	usT4     *prometheus.CounterVec
}

func newMetrics(reg *prometheus.Registry) *Metrics {
	g := func(name, help string, labels ...string) *prometheus.GaugeVec {
		v := prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: name, Help: help}, labels)
		reg.MustRegister(v)
		return v
	}
	c := func(name, help string, labels ...string) *prometheus.CounterVec {
		v := prometheus.NewCounterVec(prometheus.CounterOpts{Name: name, Help: help}, labels)
		reg.MustRegister(v)
		return v
	}
	single := func(name, help string) prometheus.Gauge {
		v := prometheus.NewGauge(prometheus.GaugeOpts{Name: name, Help: help})
		reg.MustRegister(v)
		return v
	}
	scnt := func(name, help string) prometheus.Counter {
		v := prometheus.NewCounter(prometheus.CounterOpts{Name: name, Help: help})
		reg.MustRegister(v)
		return v
	}
	return &Metrics{
		up:            single("modem_up", "1 if the last scrape of the modem succeeded"),
		scrapeSeconds: single("modem_scrape_duration_seconds", "Duration of the last modem scrape"),
		loginTotal:    scnt("modem_login_total", "Number of login attempts made"),
		loginFailed:   scnt("modem_login_failed_total", "Number of failed login attempts"),

		dsFreq:   g("modem_downstream_freq_hz", "Downstream channel frequency (Hz)", "chid"),
		dsPower:  g("modem_downstream_power_dbmv", "Downstream received power (dBmV)", "chid"),
		dsSNR:    g("modem_downstream_snr_db", "Downstream SNR (dB)", "chid"),
		dsRxMER:  g("modem_downstream_rxmer_db", "Downstream RxMER (dB)", "chid"),
		dsLocked: g("modem_downstream_locked", "Downstream channel fully locked (1) or not (0)", "chid"),
		dsPreRaw: g("modem_downstream_prers_errors_raw", "Downstream pre-RS errors (raw cumulative counter)", "chid"),
		dsPstRaw: g("modem_downstream_postrs_errors_raw", "Downstream post-RS errors (raw cumulative counter)", "chid"),
		dsPre:    c("modem_downstream_prers_errors_total", "Downstream pre-RS errors (monotonic, reset-safe)", "chid"),
		dsPst:    c("modem_downstream_postrs_errors_total", "Downstream post-RS errors (monotonic, reset-safe)", "chid"),

		usFreq:  g("modem_upstream_freq_hz", "Upstream channel frequency (Hz)", "usid"),
		usPower: g("modem_upstream_power_dbmv", "Upstream transmit power (dBmV)", "usid"),
		usSRate: g("modem_upstream_symbol_rate_ksps", "Upstream symbol rate (ksps)", "usid"),
		usT1Raw: g("modem_upstream_t1_timeouts_raw", "Upstream T1 timeouts (raw cumulative)", "usid"),
		usT2Raw: g("modem_upstream_t2_timeouts_raw", "Upstream T2 timeouts (raw cumulative)", "usid"),
		usT3Raw: g("modem_upstream_t3_timeouts_raw", "Upstream T3 timeouts (raw cumulative)", "usid"),
		usT4Raw: g("modem_upstream_t4_timeouts_raw", "Upstream T4 timeouts (raw cumulative)", "usid"),
		usT1:    c("modem_upstream_t1_timeouts_total", "Upstream T1 timeouts (monotonic, reset-safe)", "usid"),
		usT2:    c("modem_upstream_t2_timeouts_total", "Upstream T2 timeouts (monotonic, reset-safe)", "usid"),
		usT3:    c("modem_upstream_t3_timeouts_total", "Upstream T3 timeouts (monotonic, reset-safe)", "usid"),
		usT4:    c("modem_upstream_t4_timeouts_total", "Upstream T4 timeouts (monotonic, reset-safe)", "usid"),
	}
}

// ---------------------------------------------------------------------------
// Collector
// ---------------------------------------------------------------------------

type Collector struct {
	cfg    Config
	client *http.Client
	m      *Metrics

	mu       sync.Mutex
	lastRaw  map[string]float64 // reset-safe delta state
	seenLogs map[string]bool    // dedup for Loki pushes

	loginFailures int       // consecutive failed logins (for backoff)
	nextLogin     time.Time // earliest time we may attempt login again
}

func newCollector(cfg Config, m *Metrics) *Collector {
	jar, _ := cookiejar.New(nil)
	return &Collector{
		cfg: cfg,
		client: &http.Client{
			Jar:     jar,
			Timeout: 15 * time.Second,
			// Do not auto-follow the login redirect to Access-denied.html,
			// so we can detect a rejected login.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		m:        m,
		lastRaw:  map[string]float64{},
		seenLogs: map[string]bool{},
	}
}

func (c *Collector) token() string {
	u, _ := url.Parse(c.cfg.ModemURL)
	for _, ck := range c.client.Jar.Cookies(u) {
		if ck.Name == "sessionToken" {
			return ck.Value
		}
	}
	return ""
}

// post sends an ordered, url-encoded body. Order matters: this firmware's
// parser requires `token` to be the FIRST parameter, so callers build the body
// by hand rather than via url.Values.Encode() (which sorts keys alphabetically
// and would push token last -> Access-denied).
func (c *Collector) post(path, body, referer string) ([]byte, int, error) {
	req, err := http.NewRequest("POST", c.cfg.ModemURL+path, strings.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Requested-With", "XMLHttpRequest")
	// This firmware gates the login CSRF-check on the Referer matching the
	// login page; getters are happy with the index referer. Do NOT send an
	// Origin header — the modem rejects the login when it is present.
	req.Header.Set("Referer", referer)
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	return respBody, resp.StatusCode, err
}

func (c *Collector) getter(fun string) ([]byte, error) {
	reqBody := "token=" + url.QueryEscape(c.token()) + "&fun=" + url.QueryEscape(fun)
	body, code, err := c.post("/xml/getter.xml", reqBody, c.cfg.ModemURL+"/index.html")
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, fmt.Errorf("getter fun=%s: http %d", fun, code)
	}
	return body, nil
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func (c *Collector) accessLevel() (int, error) {
	reqBody := "token=" + url.QueryEscape(c.token()) + "&fun=1"
	body, code, err := c.post("/xml/getter.xml", reqBody, c.cfg.ModemURL+"/index.html")
	if err != nil {
		return 0, err
	}
	// A redirect (to login / Access-denied) means we simply are not
	// authenticated yet — report level 0 so the caller logs in.
	if code == http.StatusFound || code == http.StatusMovedPermanently {
		return 0, nil
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("fun=1: http %d", code)
	}
	lvl, _ := strconv.Atoi(firstTagValue(body, "AccessLevel"))
	return lvl, nil
}

func (c *Collector) login() error {
	c.m.loginTotal.Inc()
	// 1) seed the sessionToken cookie
	if _, err := c.client.Get(c.cfg.ModemURL + "/common_page/login.html"); err != nil {
		c.m.loginFailed.Inc()
		return fmt.Errorf("seed: %w", err)
	}
	// 2) authenticate — token MUST be first, then fun, Username, Password.
	reqBody := "token=" + url.QueryEscape(c.token()) +
		"&fun=15&Username=NULL&Password=" + sha256hex(c.cfg.Password)
	body, code, err := c.post("/xml/setter.xml", reqBody, c.cfg.ModemURL+"/common_page/login.html")
	if err != nil {
		c.m.loginFailed.Inc()
		return fmt.Errorf("login post: %w", err)
	}
	// 3) verify: AccessLevel must be non-zero
	lvl, err := c.accessLevel()
	if err != nil {
		c.m.loginFailed.Inc()
		return fmt.Errorf("verify: %w", err)
	}
	if lvl == 0 {
		c.m.loginFailed.Inc()
		hint := ""
		if code == http.StatusFound || bytes.Contains(body, []byte("Access-denied")) {
			hint = " (Access-denied — another admin session is probably active; log out of the web panel)"
		}
		return fmt.Errorf("login rejected, AccessLevel=0%s", hint)
	}
	log.Printf("login ok (AccessLevel=%d)", lvl)
	return nil
}

// delta feeds a reset-safe monotonic counter from the modem's cumulative value.
func (c *Collector) delta(cv *prometheus.CounterVec, key, label string, cur float64) {
	c.mu.Lock()
	last, ok := c.lastRaw[key]
	c.lastRaw[key] = cur
	c.mu.Unlock()
	if !ok {
		return // first observation: don't emit the whole historical total
	}
	d := cur - last
	if d < 0 {
		d = cur // modem rebooted / counter reset
	}
	if d > 0 {
		cv.WithLabelValues(label).Add(d)
	}
}

func (c *Collector) scrapeDownstream() error {
	body, err := c.getter("10")
	if err != nil {
		return err
	}
	n := 0
	err = decodeElements(body, "downstream", func(d dsChan) {
		n++
		id := strings.TrimSpace(d.ChID)
		if id == "" {
			return
		}
		if v, ok := pf(d.Freq); ok {
			c.m.dsFreq.WithLabelValues(id).Set(v)
		}
		if v, ok := pf(d.Pow); ok {
			c.m.dsPower.WithLabelValues(id).Set(v)
		}
		if v, ok := pf(d.SNR); ok {
			c.m.dsSNR.WithLabelValues(id).Set(v)
		}
		if v, ok := pf(d.RxMER); ok {
			c.m.dsRxMER.WithLabelValues(id).Set(v)
		}
		locked := 0.0
		if d.IsQamLocked == "1" && d.IsFECLocked == "1" && d.IsMpegLocked == "1" {
			locked = 1
		}
		c.m.dsLocked.WithLabelValues(id).Set(locked)
		if v, ok := pf(d.PreRs); ok {
			c.m.dsPreRaw.WithLabelValues(id).Set(v)
			c.delta(c.m.dsPre, "dspre_"+id, id, v)
		}
		if v, ok := pf(d.PostRs); ok {
			c.m.dsPstRaw.WithLabelValues(id).Set(v)
			c.delta(c.m.dsPst, "dspst_"+id, id, v)
		}
	})
	if err != nil {
		return fmt.Errorf("parse downstream: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("no downstream channels in response")
	}
	return nil
}

func (c *Collector) scrapeUpstream() error {
	body, err := c.getter("11")
	if err != nil {
		return err
	}
	err = decodeElements(body, "upstream", func(u usChan) {
		id := strings.TrimSpace(u.USID)
		if id == "" {
			return
		}
		if v, ok := pf(u.Freq); ok {
			c.m.usFreq.WithLabelValues(id).Set(v)
		}
		if v, ok := pf(u.Power); ok {
			c.m.usPower.WithLabelValues(id).Set(v)
		}
		if v, ok := pf(u.SRate); ok {
			c.m.usSRate.WithLabelValues(id).Set(v)
		}
		type to struct {
			raw *prometheus.GaugeVec
			cv  *prometheus.CounterVec
			key string
			val string
		}
		for _, t := range []to{
			{c.m.usT1Raw, c.m.usT1, "t1_" + id, u.T1Timeouts},
			{c.m.usT2Raw, c.m.usT2, "t2_" + id, u.T2Timeouts},
			{c.m.usT3Raw, c.m.usT3, "t3_" + id, u.T3Timeouts},
			{c.m.usT4Raw, c.m.usT4, "t4_" + id, u.T4Timeouts},
		} {
			if v, ok := pf(t.val); ok {
				t.raw.WithLabelValues(id).Set(v)
				c.delta(t.cv, t.key, id, v)
			}
		}
	})
	if err != nil {
		return fmt.Errorf("parse upstream: %w", err)
	}
	return nil
}

func (c *Collector) scrapeEventLog() {
	body, err := c.getter("13")
	if err != nil {
		log.Printf("eventlog: %v", err)
		return
	}
	var fresh []eventLog
	_ = decodeElements(body, "eventlog", func(e eventLog) {
		key := e.Time + "|" + e.Text
		c.mu.Lock()
		seen := c.seenLogs[key]
		c.seenLogs[key] = true
		c.mu.Unlock()
		if !seen {
			fresh = append(fresh, e)
		}
	})
	if len(fresh) > 0 && c.cfg.LokiURL != "" {
		if err := c.pushLoki(fresh); err != nil {
			log.Printf("loki push: %v", err)
		}
	}
}

// backoffDelay returns the re-login delay for the current failure streak:
// BackoffBase doubled per consecutive failure, capped at BackoffMax. This keeps
// the collector from hammering the login endpoint during an outage/lockout — the
// modem's brute-force protection blocks logins after too many failed attempts,
// which would otherwise turn a brief hiccup into a self-sustaining lockout.
func (c *Collector) backoffDelay() time.Duration {
	d := c.cfg.BackoffBase
	for i := 1; i < c.loginFailures && d < c.cfg.BackoffMax; i++ {
		d *= 2
	}
	if d > c.cfg.BackoffMax {
		d = c.cfg.BackoffMax
	}
	return d
}

func (c *Collector) scrapeOnce() error {
	// Ensure we still have a session; re-login if the modem dropped us.
	lvl, err := c.accessLevel()
	if err != nil || lvl == 0 {
		// Respect the backoff window so repeated failures don't trip the
		// modem's brute-force login lockout.
		if wait := time.Until(c.nextLogin); wait > 0 {
			return fmt.Errorf("login backoff: %s until next attempt (after %d failed logins)", wait.Round(time.Second), c.loginFailures)
		}
		if err := c.login(); err != nil {
			c.loginFailures++
			delay := c.backoffDelay()
			c.nextLogin = time.Now().Add(delay)
			return fmt.Errorf("%w; next login attempt in %s", err, delay)
		}
		c.loginFailures = 0
		c.nextLogin = time.Time{}
	}
	if err := c.scrapeDownstream(); err != nil {
		return err
	}
	if err := c.scrapeUpstream(); err != nil {
		return err
	}
	c.scrapeEventLog()
	return nil
}

func (c *Collector) run(ctx context.Context) {
	t := time.NewTicker(c.cfg.Poll)
	defer t.Stop()
	do := func() {
		start := time.Now()
		if err := c.scrapeOnce(); err != nil {
			c.m.up.Set(0)
			log.Printf("scrape failed: %v", err)
		} else {
			c.m.up.Set(1)
		}
		c.m.scrapeSeconds.Set(time.Since(start).Seconds())
	}
	do()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			do()
		}
	}
}

// ---------------------------------------------------------------------------
// Loki
// ---------------------------------------------------------------------------

type lokiStream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}
type lokiPush struct {
	Streams []lokiStream `json:"streams"`
}

func (c *Collector) pushLoki(events []eventLog) error {
	// Use the ingest time as the Loki timestamp (the modem's clock resets to
	// 1970 on reboot, and Loki rejects out-of-order/old stamps). The modem's
	// own timestamp is preserved in the log line itself.
	byLevel := map[string][][2]string{}
	for _, e := range events {
		lvl := strings.ToLower(strings.TrimSpace(e.Prior))
		if lvl == "" {
			lvl = "unknown"
		}
		ts := strconv.FormatInt(time.Now().UnixNano(), 10)
		line := fmt.Sprintf("%s [%s] %s", strings.TrimSpace(e.Time), lvl, strings.TrimSpace(e.Text))
		byLevel[lvl] = append(byLevel[lvl], [2]string{ts, line})
	}
	var push lokiPush
	for lvl, vals := range byLevel {
		push.Streams = append(push.Streams, lokiStream{
			Stream: map[string]string{"job": "modem", "source": "eventlog", "level": lvl},
			Values: vals,
		})
	}
	buf, err := json.Marshal(push)
	if err != nil {
		return err
	}
	resp, err := http.Post(c.cfg.LokiURL+"/loki/api/v1/push", "application/json", bytes.NewReader(buf))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("loki http %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// ---------------------------------------------------------------------------

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	cfg := loadConfig()
	reg := prometheus.NewRegistry()
	m := newMetrics(reg)
	col := newCollector(cfg, m)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go col.run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	log.Printf("modem-exporter listening on %s, polling %s every %s", cfg.ListenAddr, cfg.ModemURL, cfg.Poll)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, mux))
}
