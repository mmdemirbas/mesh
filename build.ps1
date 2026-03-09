$ErrorActionPreference = "Stop"

# Change to the project directory
Set-Location -Path $PSScriptRoot

New-Item -ItemType Directory -Force -Path "bin" | Out-Null
Remove-Item -Path "bin\*" -Recurse -Force -ErrorAction SilentlyContinue

Write-Host "🔨 Building for macOS M1 (Darwin/ARM64)..." -ForegroundColor Cyan
$env:GOOS = "darwin"
$env:GOARCH = "arm64"
go build -o bin\mesh-darwin-arm64 ./cmd/mesh/

Write-Host "🔨 Building for Linux/WSL (Linux/AMD64)..." -ForegroundColor Cyan
$env:GOOS = "linux"
$env:GOARCH = "amd64"
go build -o bin\mesh-linux-amd64 ./cmd/mesh/

Write-Host "🔨 Building for Windows (Windows/AMD64)..." -ForegroundColor Cyan
$env:GOOS = "windows"
$env:GOARCH = "amd64"
go build -o bin\mesh-windows-amd64.exe ./cmd/mesh/

# Reset environment variables
Remove-Item Env:\GOOS
Remove-Item Env:\GOARCH

Write-Host "✅ Build complete! Binaries are located in the bin/ directory:" -ForegroundColor Green
Get-ChildItem -Path "bin" | Select-Object Name, @{Name="Size(MB)";Expression={"{0:N2}" -f ($_.Length / 1MB)}}
