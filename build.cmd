@echo off
setlocal EnableDelayedExpansion

:: Change to the project directory
cd /d "%~dp0"

if not exist bin mkdir bin
del /q /f bin\*

echo 🔨 Building for macOS M1 (Darwin/ARM64)...
set GOOS=darwin
set GOARCH=arm64
go build -o bin\mesh-darwin-arm64 ./cmd/mesh/
if %ERRORLEVEL% neq 0 exit /b %ERRORLEVEL%

echo 🔨 Building for Linux/WSL (Linux/AMD64)...
set GOOS=linux
set GOARCH=amd64
go build -o bin\mesh-linux-amd64 ./cmd/mesh/
if %ERRORLEVEL% neq 0 exit /b %ERRORLEVEL%

echo 🔨 Building for Windows (Windows/AMD64)...
set GOOS=windows
set GOARCH=amd64
go build -o bin\mesh-windows-amd64.exe ./cmd/mesh/
if %ERRORLEVEL% neq 0 exit /b %ERRORLEVEL%

:: Reset environment variables
set GOOS=
set GOARCH=

echo ✅ Build complete! Binaries are located in the bin/ directory:
dir bin\
