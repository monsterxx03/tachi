#!/bin/sh
# sudo's askpass helper (SUDO_ASKPASS): raise a dialog, print the password to stdout. That is
# the whole contract — sudo reads the first line of stdout, so nothing else may be printed.
#
# Tachi installs this on first use (see agent/tools/askpass.go); the file here is the master
# copy, and the installed one is replaced whenever it differs. Why it exists at all: every
# command Tachi runs is detached from any controlling terminal, and sudo can then only read a
# password from an askpass program — a terminal prompt is not merely unlikely, it is impossible.
#
# Linux draws the dialog with pinentry (ships with gnupg; the gnome3/gtk/qt front ends are the
# ones with a window), speaking the Assuan protocol: SETTITLE/SETPROMPT/GETPIN, then one "D "
# line back. macOS has nothing to install — osascript is the one prompt a process without a
# terminal can raise there.
set -eu

prompt=${1:-Password}

case "$(uname -s)" in
Darwin)
  # The prompt lands inside an AppleScript string literal, so its backslashes and quotes have
  # to be escaped first. A cancelled dialog makes osascript exit non-zero, which sudo reads as
  # "no password given" — exactly what cancelling should mean.
  escaped=$(printf '%s' "$prompt" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')
  osascript \
    -e "display dialog \"$escaped\" default answer \"\" with hidden answer buttons {\"OK\"} default button \"OK\" with title \"Tachi\"" \
    -e 'text returned of result'
  ;;
*)
  pinentry=${PINENTRY:-}
  if [ -z "$pinentry" ]; then
    for candidate in pinentry-gnome3 pinentry-gtk-2 pinentry-gtk pinentry-qt pinentry; do
      if command -v "$candidate" >/dev/null 2>&1; then
        pinentry=$candidate
        break
      fi
    done
  fi
  # Checked even when PINENTRY named one: an empty answer is worse than no answer, because sudo
  # reads it as a wrong password (and keeps asking) rather than as "there is no dialog here".
  if [ -z "$pinentry" ] || ! command -v "$pinentry" >/dev/null 2>&1; then
    echo "askpass: no pinentry to draw a dialog with (gnupg ships one)" >&2
    exit 1
  fi

  secret=$(
    {
      printf 'SETTITLE %s\n' 'Tachi 需要密码'
      printf 'SETPROMPT %s\n' "$prompt"
      printf 'GETPIN\n'
      printf 'BYE\n'
    } |
      "$pinentry" |
      sed -n 's/^D //p' |
      # Assuan's escaping covers only %, CR and LF — a space is literal (measured against
      # pinentry). %25 must be unescaped LAST, or a password that literally contains "%0A"
      # comes back as a newline.
      sed -e 's/%0A/\n/g' -e 's/%0D/\r/g' -e 's/%25/%/g'
  )

  # Nothing came back: the user cancelled, or the dialog never appeared. Exiting quietly with a
  # non-zero status is what sudo reads as "no password was provided"; printing an empty line
  # would read as an empty password instead.
  [ -n "$secret" ] || exit 1
  printf '%s\n' "$secret"
  ;;
esac
