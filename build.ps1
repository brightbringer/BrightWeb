$ErrorActionPreference = 'Stop'
$env:CGO_ENABLED = '0'
Write-Host "Compilando BrightWeb.exe..." -ForegroundColor Cyan
go build -ldflags="-H=windowsgui" -o BrightWeb.exe ./cmd/ytbrowser
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
Write-Host "BrightWeb.exe compilado com sucesso!" -ForegroundColor Green
