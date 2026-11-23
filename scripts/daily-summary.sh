#!/usr/bin/env bash
# Daily DOCSIS line + internet-reachability health summary.
# Queries the local Prometheus (via the modem-prometheus container) over the
# last 24h and emits a one-line verdict plus a short detail block.
#
# Delivery: always appends to LOG and overwrites LATEST; best-effort desktop
# notification via notify-send. Run from cron (see install note at bottom).
set -euo pipefail

PROM_CTR="${PROM_CTR:-modem-prometheus}"
WIN="${WIN:-24h}"
OUTDIR="${OUTDIR:-/home/szymon/priv/modem-monitor}"
LOG="$OUTDIR/daily-health.log"
LATEST="$OUTDIR/daily-health-latest.txt"

# Query Prometheus and extract the first scalar value (handles sci-notation);
# prints empty string if no data.
q() {
  docker exec "$PROM_CTR" promtool query instant http://localhost:9090 "$1" 2>/dev/null \
    | grep -oE '=>[[:space:]]+-?[0-9.]+([eE][+-]?[0-9]+)?' | head -1 | sed -E 's/=>[[:space:]]+//'
}
# Round a possibly-empty number to N decimals; "n/a" if empty.
r() { awk -v v="$1" -v n="${2:-1}" 'BEGIN{ if(v=="") print "n/a"; else printf "%.*f", n, v }'; }

# ---- DOCSIS signal (the "cause") ----
POSTRS_AVG=$(q "avg_over_time( (sum(rate(modem_downstream_postrs_errors_total[10m])))[${WIN}:5m] )")
POSTRS_MAX=$(q "max_over_time( (sum(rate(modem_downstream_postrs_errors_total[10m])))[${WIN}:5m] )")
SNR_MIN=$(q "min_over_time( (min(modem_downstream_snr_db))[${WIN}:5m] )")
US_MAX=$(q "max_over_time( (max(modem_upstream_power_dbmv))[${WIN}:5m] )")
UP_PCT=$(q "avg_over_time(modem_up[${WIN}])*100")
T3T4=$(q "increase(sum(modem_upstream_t3_timeouts_total)[${WIN}:5m]) + increase(sum(modem_upstream_t4_timeouts_total)[${WIN}:5m])")

# ---- Reachability (the "effect") ----
loss() { q "(1 - avg_over_time(probe_success{instance=\"$1\"}[${WIN}]))*100"; }
lat()  { q "avg_over_time(probe_icmp_duration_seconds{phase=\"rtt\",instance=\"$1\"}[${WIN}])*1000"; }
L_CF=$(loss 1.1.1.1); L_GG=$(loss 8.8.8.8); L_GW=$(loss 192.168.0.1); L_KD=$(loss kakol.dev)
L_KT=$(q "(1 - avg_over_time(probe_success{instance=\"kakol.dev:443\"}[${WIN}]))*100")
# Internet loss = the better anchor (so one anchor's hiccup doesn't false-alarm).
NET_LOSS=$(awk -v a="${L_CF:-}" -v b="${L_GG:-}" 'BEGIN{ if(a==""&&b==""){print "";exit}
  if(a==""){print b;exit} if(b==""){print a;exit} print (a<b)?a:b }')
LAT_CF=$(lat 1.1.1.1)

# ---- Verdict ----
verdict=$(awk -v pr="${POSTRS_AVG:-0}" -v nl="${NET_LOSS:-0}" -v snr="${SNR_MIN:-99}" 'BEGIN{
  if (pr+0>50 || nl+0>5 || snr+0<33) print "DEGRADED";
  else if (pr+0>5 || nl+0>1) print "WATCH";
  else print "HEALTHY"; }')
case "$verdict" in HEALTHY) icon="✅";; WATCH) icon="🟡";; *) icon="🔴";; esac

STAMP=$(date '+%Y-%m-%d %H:%M')
ONELINE="$icon [$STAMP] Modem ${WIN}: $verdict — uncorr err $(r "$POSTRS_AVG" 2)/s (peak $(r "$POSTRS_MAX" 0)), SNR min $(r "$SNR_MIN" 0)dB, up-pwr max $(r "$US_MAX" 0)dBmV, net loss $(r "$NET_LOSS" 2)% @ $(r "$LAT_CF" 0)ms, kakol.dev $([ "${L_KT:-1}" = "0" ] && echo up || echo "loss $(r "$L_KT" 1)%")"

DETAIL="$ONELINE
    signal : postRS avg $(r "$POSTRS_AVG" 2)/s  peak $(r "$POSTRS_MAX" 0)/s  SNRmin $(r "$SNR_MIN" 0)dB  USmax $(r "$US_MAX" 0)dBmV  T3/T4 $(r "$T3T4" 0)  modem_up $(r "$UP_PCT" 1)%
    reach  : loss  1.1.1.1 $(r "$L_CF" 2)%  8.8.8.8 $(r "$L_GG" 2)%  gw $(r "$L_GW" 2)%  kakol.dev(icmp) $(r "$L_KD" 2)%  kakol.dev:443 $(r "$L_KT" 2)%
             rtt   1.1.1.1 $(r "$LAT_CF" 0)ms"

mkdir -p "$OUTDIR"
printf '%s\n' "$ONELINE" >> "$LOG"
printf '%s\n' "$DETAIL" > "$LATEST"
printf '%s\n' "$DETAIL"

# ---- Desktop notification (KDE/Wayland; robust under a minimal cron env) ----
# Pull the live session env straight from the running desktop so cron uses the
# SAME bus/display the user is logged into (survives reboots / session changes).
_U=$(id -u)
_PID=$(pgrep -u "$_U" -x plasmashell 2>/dev/null | head -1 || true)
if [ -n "${_PID:-}" ] && [ -r "/proc/$_PID/environ" ]; then
  while IFS= read -r -d '' _kv; do
    case "$_kv" in
      DBUS_SESSION_BUS_ADDRESS=*|DISPLAY=*|WAYLAND_DISPLAY=*|XDG_RUNTIME_DIR=*|XAUTHORITY=*) export "$_kv" ;;
    esac
  done < "/proc/$_PID/environ"
fi
# Sensible fallbacks if the desktop wasn't found.
export XDG_RUNTIME_DIR="${XDG_RUNTIME_DIR:-/run/user/$_U}"
export DBUS_SESSION_BUS_ADDRESS="${DBUS_SESSION_BUS_ADDRESS:-unix:path=/run/user/$_U/bus}"
export DISPLAY="${DISPLAY:-:0}"

if command -v notify-send >/dev/null 2>&1; then
  if [ "$verdict" = DEGRADED ]; then
    # Persistent (stays until dismissed) so a real problem can't be missed.
    notify-send -u critical -t 0 -a "Modem Monitor" "$icon Modem $verdict" "$ONELINE" 2>/dev/null || true
  else
    notify-send -u normal -t 20000 -a "Modem Monitor" "$icon Modem $verdict" "$ONELINE" 2>/dev/null || true
  fi
fi
# KDE-native passive popup as a reliable second channel on Wayland.
command -v kdialog >/dev/null 2>&1 && \
  kdialog --title "Modem Monitor — $verdict" --passivepopup "$ONELINE" 12 >/dev/null 2>&1 || true

# ---- Auto-retire this daily cron once the line has proven stable ----
# Goal: stop the daily notification a few days after the 2026-11-21 cable fix,
# but ONLY if the line actually held. On/after RETIRE_ON, if the log shows no
# WATCH/DEGRADED verdict (every run was HEALTHY), remove our own two-line cron
# block and send a final "retired" notice. If the line wobbled, stay on.
# The desktop-env vars exported above are reused here for the notification.
RETIRE_ON="${RETIRE_ON:-2026-11-28}"
_today=$(date +%F)   # YYYY-MM-DD sorts lexically == chronologically
if [[ "$_today" > "$RETIRE_ON" || "$_today" == "$RETIRE_ON" ]] \
   && command -v crontab >/dev/null 2>&1 \
   && crontab -l 2>/dev/null | grep -q 'daily-summary\.sh'; then
  if grep -Eq 'WATCH|DEGRADED' "$LOG" 2>/dev/null; then
    : # line showed instability since the fix — keep watching, do not retire
  else
    # Strip only our own lines (comment + schedule); leave other crontab entries.
    { crontab -l 2>/dev/null | grep -vE 'modem-daily-summary|daily-summary\.sh'; } \
      | crontab - || true
    _MSG="Line stable through $_today — daily DOCSIS monitor retired (cron removed). Grafana dashboards stay live at http://localhost:3001."
    printf '%s\n' "✅ [$STAMP] $_MSG" >> "$LOG"
    command -v notify-send >/dev/null 2>&1 && \
      notify-send -u normal -t 20000 -a "Modem Monitor" "✅ Monitor retired" "$_MSG" 2>/dev/null || true
    command -v kdialog >/dev/null 2>&1 && \
      kdialog --title "Modem Monitor — retired" --passivepopup "$_MSG" 12 >/dev/null 2>&1 || true
  fi
fi
