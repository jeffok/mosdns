#!/bin/sh
# Shared POSIX-sh runtime helpers for schedule and last-known-good snapshots.
# Source this file; it does not execute anything when sourced.

RUNTIME_MANAGED_RULES="accept-extra.txt ai-domains.txt apple-cn.txt bogus-ip.txt bogus-nxdomain.china.conf cn-exceptions.txt cn-domains.txt custom-hosts.txt custom-local.txt gfw-domains.txt IPv4.China.list proxy-domains.txt"
RUNTIME_REQUIRED_RULES="cn-domains.txt IPv4.China.list gfw-domains.txt proxy-domains.txt"

runtime_next_rules() {
  _rnr_now=${1:?runtime_next_rules requires epoch seconds}
  case $_rnr_now in *[!0-9]*|'') return 2 ;; esac
  _rnr_day=$(date -d "@$_rnr_now" '+%Y-%m-%d' 2>/dev/null) || return 1
  _rnr_candidate=$(date -d "$_rnr_day 04:30:00" '+%s' 2>/dev/null) || return 1
  case $_rnr_candidate in *[!0-9]*|'') return 1 ;; esac
  if [ "$_rnr_candidate" -le "$_rnr_now" ]; then
    # BusyBox date does not parse relative calendar phrases. Advancing one
    # epoch day only selects the next local date; the 04:30 epoch is then
    # recomputed from that date, so DST changes do not shift the target hour.
    _rnr_tomorrow=$(date -d "@$((_rnr_candidate + 86400))" '+%Y-%m-%d' 2>/dev/null) || return 1
    _rnr_candidate=$(date -d "$_rnr_tomorrow 04:30:00" '+%s' 2>/dev/null) || return 1
  fi
  printf '%s\n' "$_rnr_candidate"
}

_runtime_has_managed_files() {
  _rhm_dir=$1
  [ -d "$_rhm_dir" ] || return 1
  for _rhm_file in $RUNTIME_REQUIRED_RULES; do
    [ -s "$_rhm_dir/$_rhm_file" ] || return 1
  done
  for _rhm_file in $RUNTIME_MANAGED_RULES; do
    [ -f "$_rhm_dir/$_rhm_file" ] || return 1
  done
  return 0
}

_runtime_validate_dir() {
  _rvd_dir=$1
  _rvd_validator=${2:-${RUNTIME_VALIDATE_SCRIPT:-}}
  _runtime_has_managed_files "$_rvd_dir" || return 1
  [ -n "$_rvd_validator" ] || return 0
  case $_rvd_validator in
    */*)
      [ -x "$_rvd_validator" ] || [ -f "$_rvd_validator" ] || return 1
      # Validation of an LKG must not compare it against itself as the relative floor.
      _rvd_empty_lkg="${TMPDIR:-/tmp}/mosdns-runtime-no-lkg.$$"
      mkdir -p "$_rvd_empty_lkg" || return 1
      if [ -x "$_rvd_validator" ]; then
        LKG_DIR="$_rvd_empty_lkg" "$_rvd_validator" --dir "$_rvd_dir"
      else
        LKG_DIR="$_rvd_empty_lkg" sh "$_rvd_validator" --dir "$_rvd_dir"
      fi
      _rvd_rc=$?
      rmdir "$_rvd_empty_lkg" 2>/dev/null || :
      return "$_rvd_rc"
      ;;
    *)
      "$_rvd_validator" "$_rvd_dir"
      ;;
  esac
}

_runtime_copy_managed() {
  _rcm_from=$1 _rcm_to=$2
  mkdir -p "$_rcm_to" || return 1
  for _rcm_file in $RUNTIME_MANAGED_RULES; do
    [ -f "$_rcm_from/$_rcm_file" ] || return 1
    cp "$_rcm_from/$_rcm_file" "$_rcm_to/$_rcm_file" || return 1
  done
}

runtime_promote_lkg() {
  _rpl_live=${1:?runtime_promote_lkg requires live dir}
  _rpl_lkg=${2:?runtime_promote_lkg requires LKG dir}
  _rpl_validator=${3:-${RUNTIME_VALIDATE_SCRIPT:-}}
  _rpl_parent=$(dirname "$_rpl_lkg")
  _rpl_base=$(basename "$_rpl_lkg")
  mkdir -p "$_rpl_parent" || return 1
  _rpl_tmp="$_rpl_parent/.${_rpl_base}.tmp.$$"
  _rpl_stamp=$(date '+%Y%m%d%H%M%S')
  _rpl_version="$_rpl_parent/.${_rpl_base}.version.${_rpl_stamp}.$$"
  _rpl_seq=0
  while [ -e "$_rpl_version" ]; do
    _rpl_seq=$((_rpl_seq + 1))
    _rpl_version="$_rpl_parent/.${_rpl_base}.version.${_rpl_stamp}.$$.${_rpl_seq}"
  done
  _rpl_link="$_rpl_parent/.${_rpl_base}.link.$$"
  rm -rf "$_rpl_tmp" "$_rpl_link"
  mkdir "$_rpl_tmp" || return 1
  if ! _runtime_copy_managed "$_rpl_live" "$_rpl_tmp" || ! _runtime_validate_dir "$_rpl_tmp" "$_rpl_validator"; then
    rm -rf "$_rpl_tmp"
    return 1
  fi
  mv "$_rpl_tmp" "$_rpl_version" || { rm -rf "$_rpl_tmp"; return 1; }
  # Publish by atomically replacing a symlink on the same filesystem.
  ln -s "$(basename "$_rpl_version")" "$_rpl_link" || { rm -rf "$_rpl_version"; return 1; }
  if [ -L "$_rpl_lkg" ]; then
    mv -fT "$_rpl_link" "$_rpl_lkg" || { rm -f "$_rpl_link"; rm -rf "$_rpl_version"; return 1; }
  elif [ -e "$_rpl_lkg" ]; then
    # One-time migration from the historical real directory to symlink snapshots.
    _rpl_old="$_rpl_parent/.${_rpl_base}.legacy.$$.${RANDOM:-0}"
    mv "$_rpl_lkg" "$_rpl_old" || { rm -f "$_rpl_link"; rm -rf "$_rpl_version"; return 1; }
    if ! mv "$_rpl_link" "$_rpl_lkg"; then
      mv "$_rpl_old" "$_rpl_lkg" 2>/dev/null || :
      rm -f "$_rpl_link"; rm -rf "$_rpl_version"
      return 1
    fi
    rm -rf "$_rpl_old"
  else
    mv "$_rpl_link" "$_rpl_lkg" || { rm -f "$_rpl_link"; rm -rf "$_rpl_version"; return 1; }
  fi
  # Keep the active snapshot; older versions are not part of the LKG contract.
  _rpl_current=$(readlink "$_rpl_lkg" 2>/dev/null || :)
  for _rpl_old in "$_rpl_parent/.$_rpl_base.version."*; do
    [ -d "$_rpl_old" ] || continue
    [ "$(basename "$_rpl_old")" = "$_rpl_current" ] && continue
    rm -rf "$_rpl_old"
  done
  return 0
}

runtime_init_lkg_if_missing() {
  _ril_live=${1:?runtime_init_lkg_if_missing requires live dir}
  _ril_lkg=${2:?runtime_init_lkg_if_missing requires LKG dir}
  _ril_validator=${3:-${RUNTIME_VALIDATE_SCRIPT:-}}
  if _runtime_validate_dir "$_ril_lkg" "$_ril_validator"; then return 0; fi
  runtime_promote_lkg "$_ril_live" "$_ril_lkg" "$_ril_validator"
}

runtime_restore_lkg() {
  _rrl_lkg=${1:?runtime_restore_lkg requires LKG dir}
  _rrl_live=${2:?runtime_restore_lkg requires live dir}
  _rrl_validator=${3:-${RUNTIME_VALIDATE_SCRIPT:-}}
  _runtime_validate_dir "$_rrl_lkg" "$_rrl_validator" || return 1
  _rrl_parent=$(dirname "$_rrl_live")
  _rrl_base=$(basename "$_rrl_live")
  mkdir -p "$_rrl_parent" "$_rrl_live" || return 1
  _rrl_stage="$_rrl_parent/.${_rrl_base}.restore.$$"
  rm -rf "$_rrl_stage"
  mkdir "$_rrl_stage" || return 1
  if ! _runtime_copy_managed "$_rrl_lkg" "$_rrl_stage"; then
    rm -rf "$_rrl_stage"
    return 1
  fi
  # Recheck the copied candidate before touching live files.
  if ! _runtime_validate_dir "$_rrl_stage" "$_rrl_validator"; then
    rm -rf "$_rrl_stage"
    return 1
  fi
  # Startup runs before mosdns serves traffic. Replace each file atomically from
  # a validated sibling staging directory; this also works when live is a bind mount.
  for _rrl_file in $RUNTIME_MANAGED_RULES; do
    if [ -f "$_rrl_stage/$_rrl_file" ]; then
      cp "$_rrl_stage/$_rrl_file" "$_rrl_live/.$_rrl_file.new.$$" || { rm -rf "$_rrl_stage"; return 1; }
      mv -f "$_rrl_live/.$_rrl_file.new.$$" "$_rrl_live/$_rrl_file" || { rm -rf "$_rrl_stage"; return 1; }
    else
      rm -f "$_rrl_live/$_rrl_file"
    fi
  done
  rm -rf "$_rrl_stage"
  return 0
}
