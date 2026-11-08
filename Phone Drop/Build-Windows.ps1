$ErrorActionPreference = 'Stop'
$projectDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$outputDir = Join-Path $projectDir 'outputs'
$exePath = Join-Path $outputDir 'PhoneDrop.exe'
$iconPath = Join-Path $projectDir 'assets\PhoneDrop.ico'
$iconScript = Join-Path $projectDir 'Set-PhoneDropIcon.ps1'
$goCommand = Get-Command go -ErrorAction SilentlyContinue
$goExe = if ($goCommand) { $goCommand.Source } else { Join-Path $env:ProgramFiles 'Go\bin\go.exe' }
if (-not (Test-Path -LiteralPath $goExe)) {
    throw 'Go 1.22 or newer is required to build PhoneDrop. Install Go, then run this script again.'
}
New-Item -ItemType Directory -Force -Path $outputDir | Out-Null
Push-Location $projectDir
try {
    & $goExe build -trimpath -ldflags '-H=windowsgui' -o $exePath .
    if ($LASTEXITCODE -ne 0) {
        throw 'Go build failed.'
    }
    $iconEmbedded = $false
    for ($attempt = 0; $attempt -lt 5 -and -not $iconEmbedded; $attempt++) {
        try {
            & $iconScript -Executable $exePath -IconFile $iconPath
            $iconEmbedded = $true
        } catch {
            if ($attempt -eq 4) { throw }
            Start-Sleep -Milliseconds 500
        }
    }
} finally {
    Pop-Location
}
Write-Host "Built: $(Join-Path $outputDir 'PhoneDrop.exe')"
