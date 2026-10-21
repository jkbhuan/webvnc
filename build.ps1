<#
.SYNOPSIS
    构建 webvnc：先编译前端，再把前端产物嵌入 Go 二进制。

.DESCRIPTION
    构建顺序不能颠倒。go:embed 在编译期读取 frontend/dist，
    所以必须先生成 dist，否则 go build 会直接报错。

.EXAMPLE
    .\build.ps1
    完整构建，输出 webvnc.exe

.EXAMPLE
    .\build.ps1 -SkipInstall -Output webvnc-dev.exe
    跳过 npm install（依赖已装好），输出到指定文件名

.EXAMPLE
    .\build.ps1 -SkipFrontend
    只重新编译 Go（前端产物没有改动时）
#>
[CmdletBinding()]
param(
    # 输出文件名
    [string]$Output = 'webvnc.exe',

    # 跳过 npm install
    [switch]$SkipInstall,

    # 跳过前端构建，直接打包已存在的 frontend/dist
    [switch]$SkipFrontend,

    # 通过 -ldflags 注入版本号
    [string]$Version = '0.1.0'
)

$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot

function Write-Step($message) {
    Write-Host ''
    Write-Host "==> $message" -ForegroundColor Cyan
}

function Assert-Command($name, $hint) {
    if (-not (Get-Command $name -ErrorAction SilentlyContinue)) {
        throw "找不到命令 '$name'。$hint"
    }
}

# --- 环境检查 -------------------------------------------------------------
Assert-Command 'go' '请先安装 Go 1.22 或更高版本: https://go.dev/dl/'

if (-not $SkipFrontend) {
    Assert-Command 'npm' '请先安装 Node.js 18 或更高版本: https://nodejs.org/'
}

# --- 前端 -----------------------------------------------------------------
if (-not $SkipFrontend) {
    Set-Location -LiteralPath (Join-Path $PSScriptRoot 'frontend')

    if (-not $SkipInstall) {
        Write-Step '安装前端依赖 (npm install)'
        npm install --no-fund --no-audit
        if ($LASTEXITCODE -ne 0) { throw 'npm install 失败' }
    }

    Write-Step '构建前端 (npm run build)'
    # 走 build 而不是 build:only：它会先跑一遍 vue-tsc 类型检查，
    # 这里保留它以便尽早发现类型错误。build.sh 用同一个脚本。
    npm run build
    if ($LASTEXITCODE -ne 0) { throw 'npm run build 失败' }

    Set-Location -LiteralPath $PSScriptRoot
}

if (-not (Test-Path (Join-Path $PSScriptRoot 'frontend/dist/index.html'))) {
    throw '找不到 frontend/dist/index.html。请先执行 npm run build，或去掉 -SkipFrontend 重新构建。'
}

# --- 后端 -----------------------------------------------------------------
Write-Step "编译 Go 二进制 ($Output)"
$env:CGO_ENABLED = '0'   # 纯 Go + syscall 调用 Win32，不需要 C 编译器

go build -trimpath -ldflags "-s -w -X main.version=$Version" -o $Output .
if ($LASTEXITCODE -ne 0) { throw 'go build 失败' }

# --- 结果 -----------------------------------------------------------------
$exe = Get-Item -LiteralPath (Join-Path $PSScriptRoot $Output)
$sizeMB = [math]::Round($exe.Length / 1MB, 2)

Write-Host ''
Write-Host "构建完成: $($exe.FullName)  ($sizeMB MB)" -ForegroundColor Green
Write-Host ''
Write-Host '运行方式:' -ForegroundColor Gray
Write-Host "  .\$Output                                 # 监听 8339，口令随机生成" -ForegroundColor Gray
Write-Host "  .\$Output -password 你的口令 -fps 20       # 指定口令和帧率" -ForegroundColor Gray
Write-Host "  .\$Output -codec jpeg                     # 强制帧内 JPEG（CPU 占用最低）" -ForegroundColor Gray
Write-Host ''
Write-Host '提示: 该 exe 已内嵌全部前端资源，可单独拷贝到其他 Windows 电脑运行。' -ForegroundColor Gray
