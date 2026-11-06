#!/bin/sh
# Run ON THE LUCKFOX: sh /tmp/luckfox-smooth.sh
# Reversible 720p25 H264 profile; never changes Wi-Fi credentials or SSH.
set -eu
config=/userdata/rkipc.ini
startup=/oem/usr/bin/RkLunch.sh
[ -f "$config" ] || { echo "Missing $config" >&2; exit 1; }
[ -w "$startup" ] || { echo "Cannot patch $startup; no changes applied" >&2; exit 1; }
backup="${config}.before-smooth-$(date +%Y%m%d-%H%M%S)"
cp -p "$config" "$backup"
startup_backup="${backup}.RkLunch.sh"
cp -p "$startup" "$startup_backup"
temporary="${config}.smooth.$$"
startup_temporary="${temporary}.startup"
trap 'rm -f "$temporary" "$startup_temporary"' EXIT HUP INT TERM
# This firmware unconditionally copies its factory INI at every boot. Restore
# its intended "copy only when missing" guard, preserving the remaining script.
awk '
/^[[:space:]]*#if \[ ! -f "\$rkipc_ini" \]; then/ {sub(/#if/,"if"); block=1; found++; print; next}
block && /^[[:space:]]*#fi[[:space:]]*$/ {sub(/#fi/,"fi"); block=0; print; next}
/^[[:space:]]*if \[ ! -f "\$rkipc_ini" \]; then/ {found++}
{print}
END {if(found!=1 || block) {print "Unexpected factory-config block; refusing patch" > "/dev/stderr"; exit 1}}
' "$startup" > "$startup_temporary"
awk '
BEGIN {
  values["width"]="1280"; values["height"]="720";
  values["output_data_type"]="H.264"; values["h264_profile"]="baseline";
  values["rc_mode"]="CBR"; values["max_rate"]="1024";
  values["mid_rate"]="768"; values["min_rate"]="256";
  values["dst_frame_rate_num"]="25"; values["dst_frame_rate_den"]="1";
  values["gop"]="25"; values["gop_mode"]="normalP";
}
/^\[/ { active=($0 ~ /^\[video\.0\][[:space:]]*$/) }
{
  key=$0; sub(/=.*/,"",key); gsub(/^[[:space:]]+|[[:space:]]+$/,"",key);
  if(active && key in values) {print key " = " values[key]; seen[key]=1}
  else print;
}
END {for(key in values) if(!seen[key]) {print "Missing video.0 key: " key > "/dev/stderr"; failure=1}; exit failure}
' "$config" > "$temporary"
# Stop only the camera, not S21appinit stop (that also kills DHCP clients).
# Wait for its old in-memory parameters to be flushed before editing the INI.
if pidof rkipc >/dev/null 2>&1; then
  killall rkipc
  count=0
  while pidof rkipc >/dev/null 2>&1; do
    count=$((count + 1))
    [ "$count" -le 20 ] || { echo "Camera did not stop; config not changed" >&2; exit 1; }
    sleep 1
  done
fi
cat "$startup_temporary" > "$startup"
# Keep the original inode/ownership, including firmware using a symlink here.
cat "$temporary" > "$config"
sync
echo "Backup: $backup"
echo "Applied: video.0 H264 baseline 1280x720 25fps GOP25 CBR1024kbps"
echo "Startup backup: $startup_backup"
echo "Restore INI: stop rkipc, then cp '$backup' '$config'"
echo "Restore boot script: cp '$startup_backup' '$startup'"
if command -v iw >/dev/null 2>&1; then
  iw dev wlan0 set power_save off || true
  iw dev wlan0 get power_save || true
  iw dev wlan0 link || true
fi
if [ -w /etc/init.d ]; then
  wifi_startup=/etc/init.d/S99dozorwifiperformance
  [ ! -f "$wifi_startup" ] || cp -p "$wifi_startup" "${backup}.wifi-startup"
  cat > "$wifi_startup" <<'WIFI'
#!/bin/sh
# Dozor: disable Wi-Fi power saving after association, without blocking boot.
[ "${1:-}" = start ] || exit 0
(
  count=0
  while [ "$count" -lt 30 ]; do
    if iw dev wlan0 link 2>/dev/null | grep -q 'Connected to'; then
      iw dev wlan0 set power_save off
      exit
    fi
    count=$((count + 1))
    sleep 1
  done
) >/tmp/dozor-wifi-performance.log 2>&1 &
WIFI
  chmod +x "$wifi_startup"
  echo "Wi-Fi power-save OFF will also be applied after boot."
fi
(trap '' HUP; exec rkipc -a /oem/usr/share/iqfiles) >/tmp/rkipc-smooth.log 2>&1 </dev/null &
sleep 2
if ! pidof rkipc >/dev/null 2>&1; then
  echo "Camera did not start; check /tmp/rkipc-smooth.log" >&2
  tail -n 20 /tmp/rkipc-smooth.log
  exit 1
fi
echo "Camera restarted. No reboot required. Check RTSP /live/0."
