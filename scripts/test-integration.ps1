# PowerShell equivalent of the make test-integration targets (step 14):
#
#   ./scripts/test-integration.ps1              — integration tests
#   ./scripts/test-integration.ps1 -Race        — with the Go race detector
#   ./scripts/test-integration.ps1 -Repeat      — 10 repeats with fresh data
#   ./scripts/test-integration.ps1 -Race -Repeat
#
# The script loads .env, starts the dedicated db-test compose service, applies
# the migrations of this commit through the migrations image and runs the
# integration-tagged tests against TEST_DATABASE_URL. The dev database and its
# volume are never touched.

param(
    [string]$EnvFile = '.env',
    [switch]$Race,
    [switch]$Repeat,
    # Full connection string; by default it is built from POSTGRES_PASSWORD
    # and TEST_POSTGRES_PORT of the environment file.
    [string]$TestDatabaseURL = ''
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

if ([string]::IsNullOrEmpty($TestDatabaseURL)) {
    $testPort = if ([System.Environment]::GetEnvironmentVariable('TEST_POSTGRES_PORT', 'Process')) {
        [System.Environment]::GetEnvironmentVariable('TEST_POSTGRES_PORT', 'Process')
    } else {
        '5433'
    }
    $password = [System.Environment]::GetEnvironmentVariable('POSTGRES_PASSWORD', 'Process')
    if ([string]::IsNullOrEmpty($password)) {
        throw 'POSTGRES_PASSWORD is required (see .env.example)'
    }
    $TestDatabaseURL = "postgres://auction:$password@localhost:$testPort/auction_test?sslmode=disable"
}

Push-Location $projectRoot
try {
    docker compose up -d --wait db-test
    if ($LASTEXITCODE -ne 0) { throw 'db-test failed to start' }

    docker compose run --rm --build migrate-test
    if ($LASTEXITCODE -ne 0) { throw 'test migrations failed' }

    $goArgs = @('test', '-p', '1', '-tags=integration')
    if ($Repeat) { $goArgs += @('-count=10') }
    if ($Race) { $goArgs += '-race' }
    $goArgs += './...'

    $env:TEST_DATABASE_URL = $TestDatabaseURL
    & go @goArgs
    exit $LASTEXITCODE
} finally {
    Pop-Location
}
