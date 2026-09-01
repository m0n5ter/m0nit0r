#!/usr/bin/env bash
#
# Builds the m0nit0r Android client and runs it on an AVD emulator.
#
#   ./scripts/run-emulator.sh                 # build, boot/reuse an AVD, install, launch
#   ./scripts/run-emulator.sh --avd Pixel_7    # use a specific AVD (created if missing)
#   ./scripts/run-emulator.sh --no-build       # reinstall the last APK without rebuilding
#   ./scripts/run-emulator.sh --logcat         # also tail the app's logcat after launch
#   ./scripts/run-emulator.sh --api 35         # system image API level for a new AVD (default 34)
#
# Requires the Android SDK (cmdline-tools, platform-tools, emulator) - the
# same thing Android Studio installs. Point ANDROID_HOME/ANDROID_SDK_ROOT at
# it, or install Android Studio once and this script finds the default
# location on Linux and macOS. This is a local dev script, not for CI (the
# repo's GitHub Actions workflow builds the APK separately, headless).

set -euo pipefail

# ── Config ───────────────────────────────────────────────────────────────
APP_ID="com.m0n5ter.monitor"
MAIN_ACTIVITY="${APP_ID}/.MainActivity"
DEFAULT_AVD_NAME="m0nit0r"
DEFAULT_API_LEVEL="34"
BOOT_TIMEOUT_SECONDS=180

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
APK_PATH="${PROJECT_DIR}/app/build/outputs/apk/debug/app-debug.apk"

AVD_NAME=""
API_LEVEL="${DEFAULT_API_LEVEL}"
DO_BUILD=1
DO_LOGCAT=0

# ── Helpers ──────────────────────────────────────────────────────────────
log()  { printf '\033[1;36m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31mError:\033[0m %s\n' "$*" >&2; exit 1; }

usage() {
    sed -n '2,15p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

while [ $# -gt 0 ]; do
    case "$1" in
        --avd) AVD_NAME="${2:?--avd needs a name}"; shift 2 ;;
        --api) API_LEVEL="${2:?--api needs a level, e.g. 34}"; shift 2 ;;
        --no-build) DO_BUILD=0; shift ;;
        --logcat) DO_LOGCAT=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "Unknown argument: $1 (see --help)" ;;
    esac
done

# ── Locate the Android SDK ──────────────────────────────────────────────
if [ -z "${ANDROID_HOME:-}" ] && [ -z "${ANDROID_SDK_ROOT:-}" ]; then
    for candidate in "$HOME/Library/Android/sdk" "$HOME/Android/Sdk"; do
        if [ -d "$candidate" ]; then
            export ANDROID_HOME="$candidate"
            break
        fi
    done
fi
SDK_ROOT="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}"
[ -n "$SDK_ROOT" ] && [ -d "$SDK_ROOT" ] || die \
    "Android SDK not found. Install Android Studio (or just the SDK command-line tools) and set ANDROID_HOME, e.g.:
  export ANDROID_HOME=\$HOME/Android/Sdk   # Linux
  export ANDROID_HOME=\$HOME/Library/Android/sdk   # macOS"
export ANDROID_SDK_ROOT="$SDK_ROOT"

cmdline_tools_bin="$(find "$SDK_ROOT/cmdline-tools" -maxdepth 2 -type d -name bin 2>/dev/null | sort | tail -1 || true)"
export PATH="$SDK_ROOT/platform-tools:$SDK_ROOT/emulator:${cmdline_tools_bin:-}:$PATH"

for tool in adb emulator avdmanager sdkmanager; do
    command -v "$tool" >/dev/null 2>&1 || die \
        "'$tool' not found under \$ANDROID_HOME ($SDK_ROOT). Install it via Android Studio's SDK Manager (SDK Platform-Tools, Android Emulator, Android SDK Command-line Tools)."
done

# ── Build ────────────────────────────────────────────────────────────────
if [ "$DO_BUILD" = 1 ]; then
    log "Building debug APK"
    (cd "$PROJECT_DIR" && ./gradlew assembleDebug)
fi
[ -f "$APK_PATH" ] || die "No APK at $APK_PATH. Run without --no-build first."

# ── Pick or create an AVD ────────────────────────────────────────────────
existing_avds="$(avdmanager list avd -c 2>/dev/null || true)"

if [ -z "$AVD_NAME" ]; then
    AVD_NAME="$(echo "$existing_avds" | head -1)"
    if [ -z "$AVD_NAME" ]; then
        AVD_NAME="$DEFAULT_AVD_NAME"
        log "No AVD found; will create '$AVD_NAME'"
    else
        log "Using existing AVD '$AVD_NAME'"
    fi
fi

if ! echo "$existing_avds" | grep -qx "$AVD_NAME"; then
    case "$(uname -m)" in
        arm64|aarch64) abi="arm64-v8a" ;;
        *)             abi="x86_64" ;;
    esac
    image="system-images;android-${API_LEVEL};google_apis;${abi}"

    log "Creating AVD '$AVD_NAME' (API $API_LEVEL, $abi) - this downloads a system image the first time"
    yes | sdkmanager --install "$image" >/dev/null
    echo no | avdmanager create avd -n "$AVD_NAME" -k "$image" --force >/dev/null
fi

# ── Boot the emulator if nothing is running yet ─────────────────────────
running_emulator="$(adb devices | awk '/^emulator-/{print $1; exit}')"

if [ -z "$running_emulator" ]; then
    log "Starting emulator '$AVD_NAME' (this can take a minute)"
    nohup emulator -avd "$AVD_NAME" -netdelay none -netspeed full \
        >"${PROJECT_DIR}/emulator.log" 2>&1 &
    disown

    adb wait-for-device
    log "Waiting for Android to finish booting..."
    waited=0
    until [ "$(adb shell getprop sys.boot_completed 2>/dev/null | tr -d '\r')" = "1" ]; do
        sleep 2
        waited=$((waited + 2))
        [ "$waited" -lt "$BOOT_TIMEOUT_SECONDS" ] || die \
            "Emulator did not finish booting within ${BOOT_TIMEOUT_SECONDS}s - see ${PROJECT_DIR}/emulator.log"
    done
    # Unlock the (keyguard-less) default AVD screen so the launched activity is visible.
    adb shell input keyevent 82 >/dev/null 2>&1 || true
else
    log "Reusing running emulator ($running_emulator)"
fi

# ── Install and launch ───────────────────────────────────────────────────
log "Installing APK"
adb install -r "$APK_PATH"

log "Launching $MAIN_ACTIVITY"
adb shell am start -n "$MAIN_ACTIVITY" >/dev/null

if [ "$DO_LOGCAT" = 1 ]; then
    log "Tailing logcat for $APP_ID (Ctrl+C to stop)"
    sleep 1
    pid="$(adb shell pidof -s "$APP_ID" || true)"
    if [ -n "$pid" ]; then
        exec adb logcat --pid="$pid"
    else
        warn "Could not find the app's PID yet; tailing unfiltered logcat instead."
        exec adb logcat
    fi
fi

log "Done. $APP_ID is running on '$AVD_NAME'."
