@echo off
REM Shortcut script to start the mesh server daemon

cd /d "%~dp0.."
REM Require a subcommand (up, down, ps, etc.)
if "%~1"=="" (
    echo Usage: server.cmd ^<subcommand^> [flags]
    echo Example: server.cmd up
    exit /b 1
)

set SUBCOMMAND=%~1
shift

echo Executing mesh server %SUBCOMMAND%...

REM Re-construct remainder of arguments after shift
set ALL_ARGS=
:loop
if "%~1"=="" goto after_loop
set ALL_ARGS=%ALL_ARGS% %1
shift
goto loop
:after_loop

go run ./cmd/mesh %SUBCOMMAND% -config configs/server.yml %ALL_ARGS%
