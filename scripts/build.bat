@echo off
REM ===========================================================================
REM  LAN Share - cross compile script (Windows)
REM
REM  Produces a Linux ARM64 static single binary, ready to scp to a Kwrt router.
REM  Usage:  scripts\build.bat [version]
REM
REM  NOTE: this file is intentionally pure ASCII.
REM  cmd.exe reads .bat files using the OEM code page (GBK on zh-CN Windows),
REM  so any UTF-8 Chinese comment here would be decoded into garbage and its
REM  first token would be executed as a command -> a wall of
REM  "'xxx' is not recognized as an internal or external command".
REM  Keep this file ASCII-only. Chinese docs belong in README.md.
REM ===========================================================================

setlocal
cd /d "%~dp0.."

set VERSION=%1
if "%VERSION%"=="" set VERSION=0.2.0

echo [1/4] tidy modules...
set GOOS=
set GOARCH=
set CGO_ENABLED=
go mod tidy
if errorlevel 1 goto :fail

echo.
echo [2/4] build native debug binary (windows/amd64)...
set GOOS=windows
set GOARCH=amd64
set CGO_ENABLED=0
go build -trimpath -ldflags "-s -w -X lanshare/internal/api.Version=%VERSION%" -o dist\lan-share.exe ./cmd/server
if errorlevel 1 goto :fail

echo.
echo [3/4] cross compile (linux/arm64)...
set GOOS=linux
set GOARCH=arm64
set CGO_ENABLED=0
go build -trimpath -ldflags "-s -w -X lanshare/internal/api.Version=%VERSION%" -o dist\lan-share ./cmd/server
if errorlevel 1 goto :fail

echo.
echo [4/4] verify target architecture...
REM This step is not optional: if the env vars above were not applied,
REM go build silently produces a Windows binary at the linux output path.
REM Shipping that to the router fails only on the router.
where python >nul 2>nul
if errorlevel 1 (
  echo   [SKIP] python not found, cannot verify. Check dist\lan-share manually:
  echo          it must be an ELF, NOT a PE/MZ file.
) else (
  uv run python "%~dp0verify-arch.py"
  if errorlevel 1 goto :fail
)

echo.
echo Build done. Artifacts:
dir /b dist\
echo.
echo Deploy:
echo   scp dist\lan-share root@10.0.0.1:/mnt/data_mmcblk0p27/lan-share/lan-share
echo   ssh root@10.0.0.1 "/etc/init.d/lan-share restart"
goto :eof

:fail
echo.
echo BUILD FAILED - see the errors above.
exit /b 1
