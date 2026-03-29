@echo off
REM Build script for Windows

echo Building talops...

if not exist build mkdir build

go build -o build\talops.exe .

if %ERRORLEVEL% == 0 (
    echo Build successful: build\talops.exe
    echo.
    echo Run with: build\talops.exe status
) else (
    echo Build failed!
    exit /b 1
)
