@echo off
setlocal

:: Get the directory of the current script
set "DIR=%~dp0"

:: Set OS
set "OS=windows"

:: Detect Architecture
set "ARCH=%PROCESSOR_ARCHITECTURE%"
if /i "%ARCH%"=="AMD64" set "ARCH=amd64"
if /i "%ARCH%"=="ARM64" set "ARCH=arm64"
if /i "%PROCESSOR_ARCHITEW6432%"=="AMD64" set "ARCH=amd64"

set "BIN=%DIR%bin\mesh-%OS%-%ARCH%.exe"

if not exist "%BIN%" (
    echo Error: Compatible binary not found.
    echo Expected to find executable at: %BIN%
    echo Please build the project first or download the correct release.
    exit /b 1
)

:: Execute the binary and pass all arguments
"%BIN%" %*
