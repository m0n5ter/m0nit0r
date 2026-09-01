#requires -version 5.1
<#
.SYNOPSIS
    Builds the m0nit0r Android client and runs it on an AVD emulator.

.EXAMPLE
    .\scripts\run-emulator.ps1
    Build, boot/reuse an AVD, install, launch.

.EXAMPLE
    .\scripts\run-emulator.ps1 -Avd Pixel_7

.EXAMPLE
    .\scripts\run-emulator.ps1 -NoBuild
    Reinstall the last APK without rebuilding.

.EXAMPLE
    .\scripts\run-emulator.ps1 -Logcat
    Also tail the app's logcat after launch.

.EXAMPLE
    .\scripts\run-emulator.ps1 -Api 35
    System image API level for a newly created AVD (default 34).

.NOTES
    Requires the Android SDK (cmdline-tools, platform-tools, emulator) - the
    same thing Android Studio installs. Point $env:ANDROID_HOME (or
    ANDROID_SDK_ROOT) at it, or install Android Studio once and this script
    finds the default location under %LOCALAPPDATA%. This is a local dev
    script, not for CI (the repo's GitHub Actions workflow builds the APK
    separately, headless).
#>
param(
    [string]$Avd = "",
    [string]$Api = "34",
    [switch]$NoBuild,
    [switch]$Logcat
)

$ErrorActionPreference = "Stop"

# ── Config ───────────────────────────────────────────────────────────────
$AppId = "com.m0n5ter.monitor"
$MainActivity = "$AppId/.MainActivity"
$DefaultAvdName = "m0nit0r"
$BootTimeoutSeconds = 180

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$ProjectDir = Split-Path -Parent $ScriptDir
$ApkPath = Join-Path $ProjectDir "app\build\outputs\apk\debug\app-debug.apk"

function Write-Step { param([string]$Message) Write-Host "==> $Message" -ForegroundColor Cyan }
function Write-Warn { param([string]$Message) Write-Host "!! $Message" -ForegroundColor Yellow }
function Die { param([string]$Message) Write-Host "Error: $Message" -ForegroundColor Red; exit 1 }

# ── Locate the Android SDK ──────────────────────────────────────────────
$sdkRoot = $env:ANDROID_HOME
if (-not $sdkRoot) { $sdkRoot = $env:ANDROID_SDK_ROOT }
if (-not $sdkRoot) {
    $candidate = Join-Path $env:LOCALAPPDATA "Android\Sdk"
    if (Test-Path $candidate) { $sdkRoot = $candidate }
}
if (-not $sdkRoot -or -not (Test-Path $sdkRoot)) {
    Die "Android SDK not found. Install Android Studio (or just the SDK command-line tools) and set ANDROID_HOME, e.g.:`n  `$env:ANDROID_HOME = `"$env:LOCALAPPDATA\Android\Sdk`""
}
$env:ANDROID_HOME = $sdkRoot
$env:ANDROID_SDK_ROOT = $sdkRoot

$cmdlineToolsBin = Get-ChildItem -Path (Join-Path $sdkRoot "cmdline-tools") -Directory -ErrorAction SilentlyContinue |
    Sort-Object Name -Descending |
    ForEach-Object { Join-Path $_.FullName "bin" } |
    Where-Object { Test-Path $_ } |
    Select-Object -First 1

$env:Path = "$sdkRoot\platform-tools;$sdkRoot\emulator;$cmdlineToolsBin;$env:Path"

$adb = Join-Path $sdkRoot "platform-tools\adb.exe"
$emulator = Join-Path $sdkRoot "emulator\emulator.exe"
$avdmanager = if ($cmdlineToolsBin) { Join-Path $cmdlineToolsBin "avdmanager.bat" } else { $null }
$sdkmanager = if ($cmdlineToolsBin) { Join-Path $cmdlineToolsBin "sdkmanager.bat" } else { $null }

foreach ($tool in @{ adb = $adb; emulator = $emulator; avdmanager = $avdmanager; sdkmanager = $sdkmanager }.GetEnumerator()) {
    if (-not $tool.Value -or -not (Test-Path $tool.Value)) {
        Die "'$($tool.Key)' not found under `$env:ANDROID_HOME ($sdkRoot). Install it via Android Studio's SDK Manager (SDK Platform-Tools, Android Emulator, Android SDK Command-line Tools)."
    }
}

# ── Build ────────────────────────────────────────────────────────────────
if (-not $NoBuild) {
    Write-Step "Building debug APK"
    Push-Location $ProjectDir
    try {
        & .\gradlew.bat assembleDebug
        if ($LASTEXITCODE -ne 0) { Die "Gradle build failed (exit $LASTEXITCODE)." }
    } finally {
        Pop-Location
    }
}
if (-not (Test-Path $ApkPath)) { Die "No APK at $ApkPath. Run without -NoBuild first." }

# ── Pick or create an AVD ────────────────────────────────────────────────
$existingAvds = & $avdmanager list avd -c 2>$null

if (-not $Avd) {
    $Avd = $existingAvds | Select-Object -First 1
    if (-not $Avd) {
        $Avd = $DefaultAvdName
        Write-Step "No AVD found; will create '$Avd'"
    } else {
        Write-Step "Using existing AVD '$Avd'"
    }
}

if ($existingAvds -notcontains $Avd) {
    $abi = if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64") { "arm64-v8a" } else { "x86_64" }
    $image = "system-images;android-$Api;google_apis;$abi"

    Write-Step "Creating AVD '$Avd' (API $Api, $abi) - this downloads a system image the first time"
    $yes = 1..20 | ForEach-Object { "y" }
    $yes | & $sdkmanager --licenses *> $null
    $yes | & $sdkmanager --install $image | Out-Null
    "no" | & $avdmanager create avd -n $Avd -k $image --force | Out-Null
}

# ── Boot the emulator if nothing is running yet ─────────────────────────
$runningEmulator = (& $adb devices) -split "`n" | Where-Object { $_ -match "^emulator-" } | Select-Object -First 1

if (-not $runningEmulator) {
    Write-Step "Starting emulator '$Avd' (this can take a minute)"
    $emuLog = Join-Path $ProjectDir "emulator.log"
    $emuErr = Join-Path $ProjectDir "emulator.err.log"
    Start-Process -FilePath $emulator `
        -ArgumentList @("-avd", $Avd, "-netdelay", "none", "-netspeed", "full") `
        -RedirectStandardOutput $emuLog -RedirectStandardError $emuErr `
        -WindowStyle Hidden | Out-Null

    & $adb wait-for-device
    Write-Step "Waiting for Android to finish booting..."
    $waited = 0
    while ((& $adb shell getprop sys.boot_completed 2>$null).Trim() -ne "1") {
        Start-Sleep -Seconds 2
        $waited += 2
        if ($waited -ge $BootTimeoutSeconds) {
            Die "Emulator did not finish booting within ${BootTimeoutSeconds}s - see $emuLog"
        }
    }
    & $adb shell input keyevent 82 | Out-Null
} else {
    Write-Step "Reusing running emulator ($runningEmulator)"
}

# ── Install and launch ───────────────────────────────────────────────────
Write-Step "Installing APK"
& $adb install -r $ApkPath
if ($LASTEXITCODE -ne 0) { Die "adb install failed (exit $LASTEXITCODE)." }

Write-Step "Launching $MainActivity"
& $adb shell am start -n $MainActivity | Out-Null

if ($Logcat) {
    Write-Step "Tailing logcat for $AppId (Ctrl+C to stop)"
    Start-Sleep -Seconds 1
    $appPid = (& $adb shell pidof -s $AppId 2>$null).Trim()
    if ($appPid) {
        & $adb logcat "--pid=$appPid"
    } else {
        Write-Warn "Could not find the app's PID yet; tailing unfiltered logcat instead."
        & $adb logcat
    }
    exit 0
}

Write-Step "Done. $AppId is running on '$Avd'."
