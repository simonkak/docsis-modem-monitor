# DOCSIS Cable Modem Monitor

A small self-hosted monitoring stack for **DOCSIS cable modems** with an
Askey / Compal (NET-DK) web interface. A Go collector logs into the modem,
scrapes the downstream/upstream signal tables and the event log, and feeds them
into **Prometheus + Loki + Grafana** — so you get continuous history of signal
levels, error rates and modem events instead of a single point-in-time reading.

Useful when you have an intermittent line fault: the dashboard captures the
whole episode (upstream power climbing, SNR dropping, error-rate spikes, ranging
timeouts) with timestamps, which is exactly the evidence an ISP needs.

```
cable modem (default http://192.168.0.1)
   │  HTTP  POST /xml/getter.xml   fun=10 downstream · 11 upstream · 13 event log
   ▼
Go collector (modem-exporter) ── Prometheus metrics ─▶ Prometheus ─▶ Grafana
                              ── event-log lines ─────▶ Loki ───────▶ Grafana (logs)
```

## Compatibility

Written against the **Askey / Compal `NET-DK` firmware** (seen on Play /
Liberty Global modems in Europe). The modem is driven through its own JS API:

- login: `POST /xml/setter.xml` with `token=<sessionToken cookie>&fun=15&Username=NULL&Password=sha256hex(password)`
- data: `POST /xml/getter.xml` with `token=…&fun=N` (10 = downstream, 11 = upstream, 13 = event log, 1 = global status)
- every response rotates a `sessionToken` CSRF cookie, which must be echoed as the **first** form field on the next request.

Other DOCSIS modems expose different endpoints — you'd need to adapt the login
flow and the XML parsing in `collector/main.go`. PRs for other firmwares welcome.

## Quick start

```sh
cp .env.example .env
# edit .env: set MODEM_PASSWORD (your modem admin password) and a Grafana password
docker compose up -d --build
```

Open Grafana at <http://localhost:3001> (login from `.env`) → dashboard
**Cable Modem (DOCSIS)**. Datasources and dashboard are provisioned
automatically.

Point it at a different modem with `MODEM_URL` in `.env`.

## What it records

Per-channel metrics, every `POLL_INTERVAL` (default 30s):

| Metric | Notes |
|---|---|
| `modem_downstream_power_dbmv{chid}` | downstream RX power |
| `modem_downstream_snr_db{chid}` / `modem_downstream_rxmer_db{chid}` | SNR / RxMER |
| `modem_downstream_prers_errors_total{chid}` / `modem_downstream_postrs_errors_total{chid}` | **reset-safe counters** (see below) |
| `modem_downstream_locked{chid}` | 1 = fully locked |
| `modem_upstream_power_dbmv{usid}` / `modem_upstream_symbol_rate_ksps{usid}` | upstream TX power / symbol rate |
| `modem_upstream_t1..t4_timeouts_total{usid}` | ranging timeouts, reset-safe |
| `modem_up`, `modem_scrape_duration_seconds`, `modem_login_total`, `modem_login_failed_total` | collector health |

**Reset-safe counters:** the modem's own error counters zero out on reboot. The
collector tracks per-channel deltas and only ever increments its Prometheus
counters, so `rate(...)` stays correct across modem reboots instead of showing
negative spikes.

The event log is pushed to **Loki** with a `level` label
(critical/error/warning/notice). The modem's own timestamp is kept inside the
log line; the Loki timestamp is ingest time (the modem clock resets to 1970 on
reboot, which Loki would otherwise reject as out-of-order).

## Operational notes

**Single admin session.** The modem allows only one admin session at a time.
While you're logged into the modem's web panel in a browser, the collector's
login is refused (`Access-denied`, seen as `modem_login_failed_total` rising and
`modem_up=0`). Log out of the panel and read values from Grafana instead. To use
the panel, `docker compose stop modem-exporter` first. Note `modem_up=0` means
"the collector can't log in", **not** "the internet is down".

**Brute-force lockout.** The modem locks out logins after too many failed
attempts, which can turn a brief hiccup into a self-sustaining lockout. The
collector therefore uses **exponential backoff** on failed logins —
`LOGIN_BACKOFF_BASE` (default 60s), doubling per consecutive failure up to
`LOGIN_BACKOFF_MAX` (default 15m) — so it retries a few times per hour, not
every 30s. If a lockout has already happened, power-cycle the modem to clear it.

**Docker subnet vs the modem IP.** The compose network is pinned to
`10.9.0.0/24` on purpose. If Docker allocates a network inside `192.168.0.0/20`,
its bridge can grab `192.168.0.1` and the container would talk to the bridge
instead of the modem. If your modem lives on `192.168.0.1`, keep the pin (or set
`default-address-pools` in `/etc/docker/daemon.json` to steer Docker away from
your LAN range).

## Dashboard thresholds

- Upstream power: green <48, yellow 48–51, **red ≥51 dBmV** (no headroom).
- Downstream SNR/RxMER: **red <33**, yellow 33–35, green ≥35 dB.
- Downstream power: green within ±10 dBmV.
- Post-RS (uncorrectable) error rate: the primary fault signal — ~0 when healthy.

## Ports

- Grafana: `3001` (host) → 3000 (container).
- Prometheus `9090`, Loki `3100`, exporter `9210` (`/metrics`) are internal to
  the compose network. Add a `ports:` mapping to expose them.

## Configuration

All via environment (see `.env.example`):

| Var | Default | Meaning |
|---|---|---|
| `MODEM_URL` | `http://192.168.0.1` | modem base URL |
| `MODEM_PASSWORD` | — (required) | modem admin password |
| `POLL_INTERVAL` | `30s` | scrape interval |
| `LOGIN_BACKOFF_BASE` | `60s` | first re-login delay after a failure |
| `LOGIN_BACKOFF_MAX` | `15m` | cap on the exponential re-login delay |

## Security

`MODEM_PASSWORD` is read only from the environment and never logged. Keep it in
`.env` (gitignored) — do not commit it. The collector talks to the modem over
plain HTTP on the LAN, exactly as the modem's own web UI does.

## License

MIT — see [LICENSE](LICENSE).
