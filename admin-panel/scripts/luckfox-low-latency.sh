#!/bin/sh
# Run on Luckfox, never on Windows. Default action only prints the proposed INI.
# apply: H.265 / 720p25 / CBR 1536 kbit/s / GOP25, with a recoverable INI backup.
# No Wi-Fi, startup-script, Windows, VPN or go2rtc changes.
set -eu
umask 077

config=/userdata/rkipc.ini
temporary="/userdata/.dozor-low-latency.$$"
backup=
needs_restart=0
complete=0

profile() {
  awk '
  function trim(s) {sub(/^[ \t]+/,"",s); sub(/[ \t]+$/,"",s); return s}
  BEGIN {
    values["width"]="1280"; values["height"]="720";
    values["rc_mode"]="CBR"; values["max_rate"]="1536";
    values["dst_frame_rate_num"]="25"; values["dst_frame_rate_den"]="1";
    values["gop"]="25"; values["gop_mode"]="normalP";
  }
  /^[ \t]*\[/ {
    active=($0 ~ /^[ \t]*\[video\.0\][ \t]*([;#].*)?$/);
    if(active) sections++;
  }
  {
    key=$0; sub(/=.*/,"",key); key=trim(key);
    value=$0; sub(/^[^=]*=/,"",value); sub(/[;#].*/,"",value); value=trim(value);
    if(active && key=="output_data_type") {codec=value; codec_count++}
    if(active && key=="max_width") max_width=value+0;
    if(active && key=="max_height") max_height=value+0;
    if(active && key=="src_frame_rate_num") src_num=value+0;
    if(active && key=="src_frame_rate_den") src_den=value+0;
    if(active && key in values) {print key " = " values[key]; seen[key]++}
    else print;
  }
  END {
    if(sections!=1 || codec_count!=1 || codec!="H.265") {
      print "Expected exactly one video.0 with H.265; refusing codec change" > "/dev/stderr";
      failure=1;
    }
    if(max_width<1280 || max_height<720 || src_den<=0 || src_num/src_den<25) {
      print "Camera limits or source frame rate do not support this profile" > "/dev/stderr";
      failure=1;
    }
    for(key in values) if(seen[key]!=1) {
      print "Missing or duplicate video.0 key: " key > "/dev/stderr";
      failure=1;
    }
    exit failure;
  }' "$1"
}

start_camera() {
  if [ -d /oem/usr/share/iqfiles ]; then
    (trap '' HUP; exec rkipc -a /oem/usr/share/iqfiles) \
      >/tmp/rkipc-low-latency.log 2>&1 </dev/null &
  else
    (trap '' HUP; exec rkipc) >/tmp/rkipc-low-latency.log 2>&1 </dev/null &
  fi
}

cleanup() {
  result=$?
  trap - EXIT HUP INT TERM
  # After an unsuccessful change, restore only while the encoder is stopped.
  # A running encoder can overwrite an INI edited behind its back on shutdown.
  if [ "$needs_restart" = 1 ] && ! pidof rkipc >/dev/null 2>&1; then
    if [ "$complete" != 1 ] && [ -n "$backup" ] && [ -f "$backup" ]; then
      if cat "$backup" > "$config"; then sync; else echo "Restore failed: $backup" >&2; fi
    fi
    start_camera
    echo "Camera restart attempted; log: /tmp/rkipc-low-latency.log" >&2
  fi
  [ ! -f "$temporary" ] || rm -f "$temporary"
  exit "$result"
}

action=${1:-preview}
case "$action" in
  preview)
    # Optional input file makes validation possible off-device, without writes.
    profile "${2:-$config}"
    exit
    ;;
  apply)
    profile "$config" >/dev/null
    ;;
  restore)
    restore_file=${2:-}
    case "$restore_file" in
      /userdata/rkipc.ini.before-low-latency-*) ;;
      *) echo "Usage: sh $0 restore /userdata/rkipc.ini.before-low-latency-..." >&2; exit 1 ;;
    esac
    [ -f "$restore_file" ] || { echo "Backup not found" >&2; exit 1; }
    ;;
  *) echo "Usage: sh $0 preview [INI] | apply | restore BACKUP" >&2; exit 1 ;;
esac

[ "$(id -u)" = 0 ] || { echo "Run as root on the Luckfox" >&2; exit 1; }
[ -w "$config" ] || { echo "Cannot write $config" >&2; exit 1; }
command -v rkipc >/dev/null || { echo "rkipc not found" >&2; exit 1; }
command -v pidof >/dev/null || { echo "pidof not found" >&2; exit 1; }

backup="${config}.before-low-latency-$(date +%Y%m%d-%H%M%S)-$$"
cp -p "$config" "$backup"
trap cleanup EXIT
trap 'exit 130' HUP INT TERM
needs_restart=1

if pidof rkipc >/dev/null 2>&1; then
  killall rkipc
  count=0
  while pidof rkipc >/dev/null 2>&1; do
    count=$((count + 1))
    [ "$count" -le 15 ] || { echo "Camera did not stop; INI unchanged" >&2; exit 1; }
    sleep 1
  done
fi

# Capture parameters after the old encoder has flushed its in-memory settings.
cp -p "$config" "$backup"
if [ "$action" = apply ]; then profile "$config" > "$temporary"
else cat "$restore_file" > "$temporary"
fi
cat "$temporary" > "$config"
sync
start_camera
sleep 2
pidof rkipc >/dev/null 2>&1 || { echo "New camera process exited; restoring backup" >&2; exit 1; }
complete=1
needs_restart=0

echo "Backup: $backup"
if [ "$action" = apply ]; then
  echo "Applied: H.265 / 1280x720 / 25 FPS / CBR1536 / GOP25"
else echo "Restored: $restore_file"
fi
echo "Rollback: sh $0 restore '$backup'"
echo "Camera restarted. No reboot required. Check go2rtc luckfox."
echo "Boot scripts are unchanged; firmware may restore its factory INI at boot."
