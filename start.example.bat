@echo off
rem Copy to start.bat next to this file and set the two folders. start.bat is
rem yours: it is not in the repository and an update does not touch it.
cd /d "%~dp0"
set TGDL_DATA_DIR=C:\path\to\data
set TGDL_DOWNLOADS_DIR=C:\path\to\downloads
set TGDL_SERVER_BIN=%~dp0core-service\tgdl-server.exe
rem This window starts the app again when it exits, so the app may restart itself
rem for an update once its downloads are done.
set TGDL_LAUNCHER_RESTARTS=1

rem Use the ffmpeg from "winget install Gyan.FFmpeg" (it can use an NVIDIA card)
rem rather than another copy that happens to be on PATH.
set FFMPEG_PATH=
if exist "%LOCALAPPDATA%\Microsoft\WinGet\Links\ffmpeg.exe" set "FFMPEG_PATH=%LOCALAPPDATA%\Microsoft\WinGet\Links\ffmpeg.exe"
if exist "%ProgramFiles%\WinGet\Links\ffmpeg.exe" set "FFMPEG_PATH=%ProgramFiles%\WinGet\Links\ffmpeg.exe"
if not defined FFMPEG_PATH for %%R in ("%LOCALAPPDATA%\Microsoft\WinGet\Packages" "%ProgramFiles%\WinGet\Packages") do for /d %%P in ("%%~R\Gyan.FFmpeg*") do for /d %%V in ("%%~P\ffmpeg-*") do if exist "%%~V\bin\ffmpeg.exe" set "FFMPEG_PATH=%%~V\bin\ffmpeg.exe"
if defined FFMPEG_PATH (echo Using ffmpeg: %FFMPEG_PATH%) else (echo WARNING: the winget ffmpeg was not found; using whatever ffmpeg is on PATH, if any.)

:run
rem Moves to a newer release if there is one, then starts. It never goes
rem backwards, and if it cannot update it starts the installed version.
powershell -NoProfile -ExecutionPolicy Bypass -File "%~dp0ix-update.ps1"
call run_safe.bat
echo.
echo The app stopped at %date% %time%. Starting it again in 30 seconds...
echo To stop it for good, close this window.
timeout /t 30 /nobreak >nul
goto run
