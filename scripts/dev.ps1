param(
    [string]$EnvFile = '.env'
)

$ErrorActionPreference = 'Stop'
$projectRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$envPath = if ([System.IO.Path]::IsPathRooted($EnvFile)) { $EnvFile } else { Join-Path $projectRoot $EnvFile }
if (-not (Test-Path -LiteralPath $envPath -PathType Leaf)) {
    throw "Environment file not found: $envPath"
}

foreach ($line in [System.IO.File]::ReadAllLines($envPath)) {
    $entry = $line.Trim()
    if ($entry.Length -eq 0 -or $entry.StartsWith('#')) { continue }
    $parts = $entry.Split('=', 2)
    if ($parts.Length -ne 2 -or $parts[0] -notmatch '^[A-Za-z_][A-Za-z0-9_]*$') {
        throw 'Invalid environment file entry'
    }
    [System.Environment]::SetEnvironmentVariable($parts[0], $parts[1], 'Process')
}

Push-Location $projectRoot
try {
    & go run ./cmd/server
    exit $LASTEXITCODE
} finally {
    Pop-Location
}
