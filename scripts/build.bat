@echo off
REM ===========================================================================
REM  LAN Share · 交叉编译脚本（Windows）
REM
REM  产出 Linux ARM64 静态单二进制，可直接 scp 到 Kwrt 路由器。
REM  用法：  scripts\build.bat
REM ===========================================================================

setlocal
cd /d "%~dp0.."

set VERSION=%1
if "%VERSION%"=="" set VERSION=0.2.0

echo [1/3] 整理依赖...
set GOOS=
set GOARCH=
set CGO_ENABLED=
go mod tidy
if errorlevel 1 goto :fail

echo.
echo [2/3] 构建本机调试版（Windows）...
go build -trimpath -ldflags "-s -w -X lanshare/internal/api.Version=%VERSION%" -o dist\lan-share.exe ./cmd/server
if errorlevel 1 goto :fail

echo.
echo [3/3] 交叉编译 Linux ARM64...
set GOOS=linux
set GOARCH=arm64
set CGO_ENABLED=0
go build -trimpath -ldflags "-s -w -X lanshare/internal/api.Version=%VERSION%" -o dist\lan-share ./cmd/server
if errorlevel 1 goto :fail

echo.
echo 构建完成，产物：
dir /b dist\
echo.
echo 部署：
echo   scp dist\lan-share root@10.0.0.1:/mnt/data_mmcblk0p27/lan-share/lan-share
echo   ssh root@10.0.0.1 "/etc/init.d/lan-share restart"
goto :eof

:fail
echo.
echo 构建失败，请检查上面的错误信息。
exit /b 1
